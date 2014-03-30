package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"io"
	log "minilog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	COMMAND_EXEC = iota
	COMMAND_FILE_SEND
	COMMAND_FILE_RECV
	COMMAND_LOG
)

type Command struct {
	ID int

	Type int

	// true if the master should record responses to disk
	Record bool

	// Command to run if type == COMMAND_EXEC
	// The command is a slice of strings with the first element being the
	// command, and any other elements as the arguments
	Command []string

	// Files to transfer to the client if type == COMMAND_EXEC | COMMAND_FILE_SEND
	// Any path given in a file specified here will be rooted at <BASE>/files
	FilesSend []string

	// Files to transfer back to the master if type == COMMAND_EXEC | COMMAND_FILE_RECV
	FilesRecv []string

	// Log level to set if type == COMMAND_LOG
	LogLevel string

	// File to output logging to if type == COMMAND_LOG
	// File logging will be disabled if LogPath == ""
	LogPath string

	// Filter for clients to process commands. Not all fields in a client
	// must be set (wildcards), but all set fields must match for a command
	// to be processed. A client may match on one or more clients in the
	// slice, which allows for filters to be processed as a logical sum of
	// products.
	Filter []*Client

	// clients that have responded to this command
	// leave this private as we don't want to bother sending this
	// downstream
	checkedIn []int64

	// conditions on which commands can expire
	ExpireClients  int
	ExpireStarted  time.Time
	ExpireDuration time.Duration
	ExpireTime     time.Time
}

type Response struct {
	// ID counter, must match the corresponding Command
	ID int

	// Names and data for uploaded files
	Files map[string][]byte

	// Output from responding command, if any
	Stdout string
	Stderr string
}

var (
	commands           map[int]*Command
	commandCounter     int
	commandLock        sync.Mutex
	commandCounterLock sync.Mutex
	updateCommandQueue chan map[int]*Command
)

func init() {
	commands = make(map[int]*Command)
	updateCommandQueue = make(chan map[int]*Command, 1024)
	go expireReaper()
}

// periodically reap commands that meet expiry conditions
func expireReaper() {
	for {
		time.Sleep(time.Duration(REAPER_RATE) * time.Second)
		log.Debugln("expireReaper")
		now := time.Now()
		commandLock.Lock()
		for k, v := range commands {
			if v.ExpireClients != 0 {
				if len(v.checkedIn) >= v.ExpireClients {
					log.Debug("expiring command %v after %v/%v checkins", k, len(v.checkedIn), v.ExpireClients)
					delete(commands, k)
				}
			} else if v.ExpireDuration != 0 {
				if time.Since(v.ExpireStarted) > v.ExpireDuration {
					log.Debug("expiring command %v after %v", k, v.ExpireDuration)
					delete(commands, k)
				}
			} else if !v.ExpireTime.IsZero() {
				if now.After(v.ExpireTime) {
					log.Debug("expiring command %v at time %v, now is %v", k, v.ExpireTime, now)
					delete(commands, k)
				}
			}
		}
		commandLock.Unlock()
	}
}

func commandCheckIn(id int, cid int64) {
	commandLock.Lock()
	if c, ok := commands[id]; ok {
		c.checkedIn = append(c.checkedIn, cid)
	}
	commandLock.Unlock()
}

func getCommandID() int {
	log.Debugln("getCommandID")
	commandCounterLock.Lock()
	defer commandCounterLock.Unlock()
	commandCounter++
	id := commandCounter
	return id
}

func getMaxCommandID() int {
	log.Debugln("getMaxCommandID")
	return commandCounter
}

func checkMaxCommandID(id int) {
	log.Debugln("checkMaxCommandID")
	commandCounterLock.Lock()
	defer commandCounterLock.Unlock()
	if id > commandCounter {
		log.Debug("found higher ID %v", id)
		commandCounter = id
	}
}

func commandDelete(id int) string {
	commandLock.Lock()
	defer commandLock.Unlock()
	if _, ok := commands[id]; ok {
		delete(commands, id)
		return fmt.Sprintf("command %v deleted", id)
	} else {
		return fmt.Sprintf("command %v not found", id)
	}
}

func shouldRecord(id int) bool {
	commandLock.Lock()
	defer commandLock.Unlock()
	if c, ok := commands[id]; ok {
		return c.Record
	}
	return false
}

func commandDeleteFiles(id int) string {
	commandLock.Lock()
	defer commandLock.Unlock()
	if _, ok := commands[id]; ok {
		path := fmt.Sprintf("%v/responses/%v", *f_base, id)
		err := os.RemoveAll(path)
		if err != nil {
			log.Errorln(err)
			return err.Error()
		}
		return fmt.Sprintf("command %v files deleted", id)
	} else {
		return fmt.Sprintf("command %v not found", id)
	}
}

func commandResubmit(id int) string {
	commandLock.Lock()
	defer commandLock.Unlock()
	if c, ok := commands[id]; ok {
		newcommand := &Command{
			ID:        getCommandID(),
			Type:      c.Type,
			Record:    c.Record,
			Command:   c.Command,
			FilesSend: c.FilesSend,
			FilesRecv: c.FilesRecv,
			LogLevel:  c.LogLevel,
			LogPath:   c.LogPath,
			Filter:    c.Filter,
		}
		commands[newcommand.ID] = newcommand
		return fmt.Sprintf("command %v resubmitted as command %v", id, newcommand.ID)
	} else {
		return fmt.Sprintf("command %v not found", id)
	}
}

func encodeCommands() []byte {
	log.Debugln("encodeCommands")
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	err := enc.Encode(commands)
	if err != nil {
		log.Errorln(err)
		return []byte{}
	}
	return buf.Bytes()
}

func handleCommands(w http.ResponseWriter, r *http.Request) {
	log.Debugln("handleCommands")
	commandLock.Lock()
	defer commandLock.Unlock()

	// get an ordered list of the command ids
	var ids []int
	for k, _ := range commands {
		ids = append(ids, k)
	}
	sort.Ints(ids)

	if len(ids) == 0 {
		resp := "<html>no commands founds</html>"
		w.Write([]byte(resp))
		return
	}

	// list the commands
	resp := "<html><table border=1><tr><td>Command ID</td><td>Type</td><td>Command</td><td>Files -> client</td><td>Files <- client</td><td>Log level</td><td>Log Path</td><td>Record Responses</td><td>Number of responses</td><td>Delete Command</td><td>Delete Command Response Files</td><td>Resubmit</td></tr>"

	for _, k := range ids {
		c := commands[k]
		deletePath := fmt.Sprintf("<a href=\"/command/delete?id=%v\">Delete Command</a>", c.ID)
		deleteFilesPath := fmt.Sprintf("<a href=\"/command/deletefiles?id=%v\">Delete Command Files</a>", c.ID)
		resubmitPath := fmt.Sprintf("<a href=\"/command/resubmit?id=%v\">Resubmit</a>", c.ID)
		resp += fmt.Sprintf("<tr><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td><td>%v</td></tr>", c.ID, c.Type, c.Command, c.FilesSend, c.FilesRecv, c.LogLevel, c.LogPath, c.Record, len(c.checkedIn), deletePath, deleteFilesPath, resubmitPath)
	}

	resp += "</table></html>"

	w.Write([]byte(resp))
}

func handleNewCommand(w http.ResponseWriter, r *http.Request) {
	log.Debugln("handleNewCommand")

	// if no args, then present the new command dialog, otherwise try to parse the input
	commandType := r.FormValue("type")
	var resp string

	ec := r.FormValue("expire_responses")
	ed := r.FormValue("expire_duration")
	et := r.FormValue("expire_time")
	expireClients, err := strconv.Atoi(ec)
	if err != nil && ec != "" {
		log.Errorln(err)
		expireClients = 0
	}
	expireDuration, err := time.ParseDuration(ed)
	if err != nil && ed != "" {
		log.Errorln(err)
	}

	now := time.Now()
	expireTime, err := time.Parse(time.Kitchen, et)
	expireTime = time.Date(now.Year(), now.Month(), now.Day(), expireTime.Hour(), expireTime.Minute(), expireTime.Second(), 0, now.Location())
	if err != nil && et != "" {
		log.Errorln(err)
	}

	log.Debug("got type %v", commandType)

	switch commandType {
	case "exec":
		commandCmd := r.FormValue("command")
		if commandCmd == "" {
			resp = "<html>no command specified</html>"
		} else {
			commandFilesSend := r.FormValue("filesend")
			commandFilesRecv := r.FormValue("filerecv")
			commandRecord := r.FormValue("record")
			var record bool
			if commandRecord == "record" {
				record = true
			}
			c := &Command{
				Type:           COMMAND_EXEC,
				Record:         record,
				ID:             getCommandID(),
				Command:        fieldsQuoteEscape(commandCmd),
				FilesSend:      strings.Fields(commandFilesSend),
				FilesRecv:      strings.Fields(commandFilesRecv),
				Filter:         getFilter(r),
				ExpireClients:  expireClients,
				ExpireStarted:  time.Now(),
				ExpireDuration: expireDuration,
				ExpireTime:     expireTime,
			}
			log.Debug("generated command %v", c)
			commandLock.Lock()
			commands[c.ID] = c
			commandLock.Unlock()
			resp = fmt.Sprintf("<html>command %v submitted</html", c.ID)
		}
	case "filesend":
		commandFilesSend := r.FormValue("filesend")
		if commandFilesSend == "" {
			resp = "<html>no files specified</html>"
		} else {
			commandRecord := r.FormValue("record")
			var record bool
			if commandRecord == "record" {
				record = true
			}
			c := &Command{
				Type:           COMMAND_FILE_SEND,
				Record:         record,
				ID:             getCommandID(),
				FilesSend:      strings.Fields(commandFilesSend),
				Filter:         getFilter(r),
				ExpireClients:  expireClients,
				ExpireStarted:  time.Now(),
				ExpireDuration: expireDuration,
				ExpireTime:     expireTime,
			}
			log.Debug("generated command %v", c)
			commandLock.Lock()
			commands[c.ID] = c
			commandLock.Unlock()
			resp = fmt.Sprintf("<html>command %v submitted</html", c.ID)
		}
	case "filerecv":
		commandFilesRecv := r.FormValue("filerecv")
		if commandFilesRecv == "" {
			resp = "<html>no files specified</html>"
		} else {
			commandRecord := r.FormValue("record")
			var record bool
			if commandRecord == "record" {
				record = true
			}
			c := &Command{
				Type:           COMMAND_FILE_RECV,
				Record:         record,
				ID:             getCommandID(),
				FilesRecv:      strings.Fields(commandFilesRecv),
				Filter:         getFilter(r),
				ExpireClients:  expireClients,
				ExpireStarted:  time.Now(),
				ExpireDuration: expireDuration,
				ExpireTime:     expireTime,
			}
			log.Debug("generated command %v", c)
			commandLock.Lock()
			commands[c.ID] = c
			commandLock.Unlock()
			resp = fmt.Sprintf("<html>command %v submitted</html", c.ID)
		}
	case "log":
		commandLogLevel := r.FormValue("loglevel")
		if commandLogLevel == "" {
			resp = "<html>no log level specified</html>"
		} else {
			commandRecord := r.FormValue("record")
			var record bool
			if commandRecord == "record" {
				record = true
			}
			c := &Command{
				Type:           COMMAND_LOG,
				Record:         record,
				ID:             getCommandID(),
				LogLevel:       commandLogLevel,
				LogPath:        r.FormValue("logpath"),
				Filter:         getFilter(r),
				ExpireClients:  expireClients,
				ExpireStarted:  time.Now(),
				ExpireDuration: expireDuration,
				ExpireTime:     expireTime,
			}
			log.Debug("generated command %v", c)
			commandLock.Lock()
			commands[c.ID] = c
			commandLock.Unlock()
			resp = fmt.Sprintf("<html>command %v submitted</html", c.ID)
		}
	default:
		resp = `
			<html>
				<form method=post action=/command/new>
					Command type: <select name=type>
						<option selected value=exec>Execute</option>
						<option value=filesend>Send Files</option>
						<option value=filerecv>Receive Files</option>
						<option value=log>Change log level</option>
					</select>
					<br>
					<input type=checkbox name=record value=record>Record Responses
					<br>
					Command: <input type=text name=command>
					<br>
					Files -> client (space delimited) <input type=text name=filesend>
					<br>
					Files <- client (space delimited) <input type=text name=filerecv>
					<br>
					New log level: <select name=loglevel>
						<option value=debug>Debug</option>
						<option value=info>Info</option>
						<option selected value=warn>Warn</option>
						<option value=error>Error</option>
						<option value=fatal>Fatal</option>
					</select>
					<br>
					Log file path: <input type=text name=logpath>
					<br>
					Filter (blank fields are wildcard):
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;CID: <input type=text name=filter_cid>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;Hostname: <input type=text name=filter_hostname>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;Arch: <input type=text name=filter_arch>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;OS: <input type=text name=filter_os>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;OS Version: <select name=filter_osver>
					<option value=""></option>
					<option value="Windows 7">Windows 7</option>
					<option value="Windows XP">Windows XP</option>
					<option value="Windows Vista">Windows Vista</option>
					<option value="Windows 8">Windows 8</option>
					<option value="Windows 8.1">Windows 8.1</option>
					<option value="Windows Server 2012">Windows Server 2012</option>
					<option value="Windows Server 2008">Windows Server 2008</option>
					<option value="Windows Server 2003">Windows Server 2003</option>
					<option value="Windows 2000">Windows 2000</option>
					<option value="Windows Longhorn">Windows Longhorn</option>
					<option value="Windows .NET Server 2003">Windows .NET Server 2003</option>
					<option value="Windows .NET Server">Windows .NET Server 2003</option>
					<option value="Windows NT 5.00">Windows NT 5.00</option>
					<option value="Windows Me">Windows Me</option>
					<option value="Windows 98">Windows 98</option>
					<option value="Windows 95">Windows 95</option>
					</select>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;CSD Version: <select name=filter_csdver>
					<option value=""></option>
					<option value="none">None</option>
					<option value="Service Pack 1">Service Pack 1</option>
					<option value="Service Pack 2">Service Pack 2</option>
					<option value="Service Pack 3">Service Pack 3</option>
					</select>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;Edition ID: <select name=filter_editionid>
					<option value=""></option>
					<option value="Starter">Starter</option>
					<option value="Home Basic">Home Basic</option>
					<option value="Home Premium">Home Premium</option>
					<option value="Professional">Professional</option>
					<option value="Enterprise">Enterprise</option>
					<option value="Ultimate">Ultimate</option>
					</select>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;IP (IP or CIDR list, space delimited): <input type=text name=filter_ip>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;MAC (space delimited): <input type=text name=filter_mac>
					Command Expiry (blank fields are unused):
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;Number of responses: <input type=text name=expire_responses>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;Duration: <input type=text name=expire_duration>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;Duration examples: (300s, 2h45m). Valid units are "s", "m", "h"
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;Time: <input type=text name=expire_time>
					<br>
					&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;Time must be in the form of "3:04PM"
					<br>
					<input type=submit value=Submit>
				</form>
			</html>`
	}

	w.Write([]byte(resp))
}

func getFilter(r *http.Request) []*Client {
	cid := r.FormValue("filter_cid")
	cidInt, err := strconv.ParseInt(cid, 10, 64)
	if err != nil {
		cidInt = 0
	}
	host := r.FormValue("filter_hostname")
	arch := r.FormValue("filter_arch")
	os := r.FormValue("filter_os")
	osver := r.FormValue("filter_osver")
	csdver := r.FormValue("filter_csdver")
	editionid := r.FormValue("filter_editionid")
	ip := r.FormValue("filter_ip")
	mac := r.FormValue("filter_mac")

	ips := strings.Fields(ip)
	macs := strings.Fields(mac)

	return []*Client{&Client{
		CID:       cidInt,
		Hostname:  host,
		Arch:      arch,
		OS:        os,
		OSVer:     osver,
		CSDVer:    csdver,
		EditionID: editionid,
		IP:        ips,
		MAC:       macs,
	}}
}

func handleDeleteCommand(w http.ResponseWriter, r *http.Request) {
	log.Debugln("handleDeleteCommand")
	id := r.FormValue("id")
	val, err := strconv.Atoi(id)
	if err != nil {
		log.Errorln(err)
		w.Write([]byte(err.Error()))
		return
	}
	resp := commandDelete(val)
	resp = fmt.Sprintf("<html>%v</html>", resp)
	w.Write([]byte(resp))
}

func handleDeleteFiles(w http.ResponseWriter, r *http.Request) {
	log.Debugln("handleDeleteFiles")
	id := r.FormValue("id")
	val, err := strconv.Atoi(id)
	if err != nil {
		log.Errorln(err)
		w.Write([]byte(err.Error()))
		return
	}
	resp := commandDeleteFiles(val)
	resp = fmt.Sprintf("<html>%v</html>", resp)
	w.Write([]byte(resp))
}

func handleResubmit(w http.ResponseWriter, r *http.Request) {
	log.Debugln("handleResubmit")
	id := r.FormValue("id")
	val, err := strconv.Atoi(id)
	if err != nil {
		log.Errorln(err)
		w.Write([]byte(err.Error()))
		return
	}
	resp := commandResubmit(val)
	resp = fmt.Sprintf("<html>%v</html>", resp)
	w.Write([]byte(resp))
}

func updateCommands(newCommands map[int]*Command) {
	log.Debugln("updateCommands")
	updateCommandQueue <- newCommands
}

func updateCommandQueueProcessor() {
	for {
		c := <-updateCommandQueue
		log.Debugln("updateCommandQueueProcessor")
		for k, v := range c {
			if len(v.FilesSend) != 0 {
				commandGetFiles(v.FilesSend)
			}

			commandLock.Lock()
			if w, ok := commands[k]; ok {
				v.checkedIn = w.checkedIn
			} else {
				log.Debug("new command %v", k)
			}
			commands[k] = v
			commandLock.Unlock()
		}
	}
}

func commandGetFiles(files []string) {
	for _, v := range files {
		log.Debug("get file %v", v)
		path := fmt.Sprintf("%vfiles/%v", *f_base, v)

		if _, err := os.Stat(path); err == nil {
			// file exists
			continue
		}

		url := fmt.Sprintf("http://%v:%v/files/%v", ronParent, ronPort, v)
		log.Debug("file get url %v", url)
		resp, err := http.Get(url)
		if err != nil {
			log.Errorln(err)
			continue
		}

		dir := filepath.Dir(path)
		err = os.MkdirAll(dir, os.FileMode(0770))
		if err != nil {
			log.Errorln(err)
			resp.Body.Close()
			continue
		}
		f, err := os.Create(path)
		if err != nil {
			log.Errorln(err)
			resp.Body.Close()
			continue
		}
		io.Copy(f, resp.Body)
		f.Close()
		resp.Body.Close()
	}
}

// Return a slice of strings, split on whitespace, not unlike strings.Fields(),
// except that quoted fields are grouped.
// 	Example: a b "c d"
// 	will return: ["a", "b", "c d"]
func fieldsQuoteEscape(input string) []string {
	f := strings.Fields(input)
	var ret []string
	trace := false
	temp := ""
	for _, v := range f {
		if trace {
			if strings.HasSuffix(v, "\"") {
				trace = false
				temp += " " + v[:len(v)-1]
				ret = append(ret, temp)
			} else {
				temp += " " + v
			}
		} else if strings.HasPrefix(v, "\"") {
			trace = true
			temp = v[1:]

		} else {
			ret = append(ret, v)
		}
	}
	return ret
}