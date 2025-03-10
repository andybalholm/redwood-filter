package main

import (
	"bytes"
	"crypto/md5"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baruwa-enterprise/clamd"
	starlarkjson "go.starlark.net/lib/json"
	"go.starlark.net/starlark"
)

var (
	accessLog   CSVLog
	tlsLog      CSVLog
	contentLog  CSVLog
	starlarkLog CSVLog
	authLog     CSVLog

	customLogs    = map[string]*CSVLog{}
	customLogLock sync.Mutex
)

type CSVLog struct {
	lock sync.Mutex
	file *os.File
	path string
	csv  *csv.Writer
}

func (l *CSVLog) Open(filename string) {
	l.lock.Lock()
	defer l.lock.Unlock()
	if l.file != nil && l.file != os.Stdout {
		l.file.Close()
		l.file = nil
		l.path = ""
	}

	if filename != "" {
		logfile, err := os.OpenFile(filename, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
		if err != nil {
			log.Printf("Could not open log file (%s): %s\n Sending log messages to standard output instead.", filename, err)
		} else {
			l.file = logfile
			l.path = filename
		}
	}
	if l.file == nil {
		l.file = os.Stdout
	}

	l.csv = csv.NewWriter(l.file)
}

func (l *CSVLog) Log(data []string) {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.csv.Write(data)
	l.csv.Flush()
}

var starlarkJSONEncode = starlarkjson.Module.Members["encode"]

func logAccess(req *http.Request, resp *http.Response, contentLength int64, pruned bool, user string, tally map[rule]int, scores map[string]int, rule ACLActionRule, title string, ignored []string, clamdResponse []*clamd.Response, extraData any) []string {
	conf := getConfig()

	modified := ""
	if pruned {
		modified = "pruned"
	}

	status := 0
	if resp != nil {
		status = resp.StatusCode
	}

	if rule.Action == "" {
		rule.Action = "allow"
	}

	var contentType string
	if resp != nil {
		contentType = resp.Header.Get("Content-Type")
	}
	if ct2, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = ct2
	}

	var userAgent string
	if conf.LogUserAgent {
		userAgent = req.Header.Get("User-Agent")
	}

	var clamdStatus string
	if len(clamdResponse) > 0 {
		r := clamdResponse[0]
		if r.Signature != "" {
			clamdStatus = r.Status + " " + r.Signature
		} else {
			clamdStatus = r.Status
		}
	}

	if len(title) > 500 {
		title = title[:500]
	}

	clientIP := req.RemoteAddr
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}

	filteredScores := scores
	if !conf.Verbose["acl-categories"] {
		filteredScores = make(map[string]int, len(scores))
		for category, score := range scores {
			if c, ok := conf.Categories[category]; ok && c.action == ACL {
				continue
			}
			filteredScores[category] = score
		}
	}

	var extraDataString string
	switch extraData := extraData.(type) {
	case nil:
		extraDataString = ""
	case starlark.Value:
		j, err := starlark.Call(&starlark.Thread{Name: "json.encode"}, starlarkJSONEncode, starlark.Tuple{extraData}, nil)
		if err != nil {
			log.Println("Error from starlark json.encode:", err)
		} else if j, ok := j.(starlark.String); ok {
			extraDataString = string(j)
		} else {
			log.Printf("Unexpected type returned from Starlark json.encode: %T", j)
		}

	default:
		if b, err := json.Marshal(extraData); err == nil {
			extraDataString = string(b)
		}
	}

	logLine := toStrings(time.Now().Format("2006-01-02 15:04:05.000000"), user, rule.Action, req.URL, req.Method, status, contentType, contentLength, modified, listTally(stringTally(tally)), listTally(filteredScores), rule.Conditions(), title, strings.Join(ignored, ","), userAgent, req.Proto, req.Referer(), platform(req.Header.Get("User-Agent")), downloadedFilename(resp), clamdStatus, rule.Description, clientIP, extraDataString)

	accessLog.Log(logLine)
	return logLine
}

func downloadedFilename(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	disposition := resp.Header.Get("Content-Disposition")
	if disposition == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		return ""
	}
	return params["filename"]
}

func logTLS(user, serverAddr, serverName string, err error, cachedCert bool, tlsFingerprint string) {
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}

	cached := ""
	if cachedCert {
		cached = "cached certificate"
	}

	tlsLog.Log(toStrings(time.Now().Format("2006-01-02 15:04:05.000000"), user, serverName, serverAddr, errStr, cached, tlsFingerprint))
}

func logContent(u *url.URL, content []byte, scores map[string]int) {
	conf := getConfig()
	if conf.ContentLogDir == "" {
		return
	}

	filename := fmt.Sprintf("%x", md5.Sum(content))
	path := filepath.Join(conf.ContentLogDir, filename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Error creating content log file (%s): %v", path, err)
		return
	}
	defer f.Close()

	topCategory, topScore := "", 0
	for c, s := range scores {
		if s > topScore && conf.Categories[c] != nil && conf.Categories[c].action != ACL {
			topCategory = c
			topScore = s
		}
	}

	f.Write(content)
	contentLog.Log([]string{u.String(), filename, topCategory, strconv.Itoa(topScore)})
}

// toStrings converts its arguments into a slice of strings.
func toStrings(a ...interface{}) []string {
	result := make([]string, len(a))
	for i, x := range a {
		result[i] = fmt.Sprint(x)
	}
	return result
}

// stringTally returns a copy of tally with strings instead of rules as keys.
func stringTally(tally map[rule]int) map[string]int {
	st := make(map[string]int)
	for r, n := range tally {
		st[r.String()] = n
	}
	return st
}

// listTally sorts the tally and formats it as a comma-separated string.
func listTally(tally map[string]int) string {
	b := new(bytes.Buffer)
	for i, rule := range sortedKeys(tally) {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprint(b, rule, " ", tally[rule])
	}
	return b.String()
}

// logVerbose logs a message with log.Printf, but only if the --verbose flag
// is turned on for the category.
func logVerbose(messageCategory string, format string, v ...interface{}) {
	if getConfig().Verbose[messageCategory] {
		log.Printf(format, v...)
	}
}

// logAuthEvent logs all remote device authentication events to the designated log file.
func logAuthEvent(
	authType string,
	status string,
	address string,
	port int,
	user string,
	pwd string,
	platform string,
	network string,
	req *http.Request,
	message string,
) {
	ua := req.Header.Get("User-Agent")
	url := req.URL
	authLog.Log(toStrings(time.Now().Format("2006-01-02 15:04:05.000000"), status, authType, address, port, user, pwd, platform, network, ua, url, message))
}

func (l *CSVLog) String() string {
	return fmt.Sprintf("CSVLog(%q)", l.path)
}

func (l *CSVLog) Type() string {
	return "CSVLog"
}

func (l *CSVLog) Freeze() {}

func (l *CSVLog) Truth() starlark.Bool { return true }

func (l *CSVLog) Hash() (uint32, error) {
	return 0, errors.New("unhashable type: CSVLog")
}

func (l *CSVLog) AttrNames() []string {
	return []string{"log"}
}

func (l *CSVLog) Attr(name string) (starlark.Value, error) {
	switch name {
	case "log":
		return starlark.NewBuiltin("log", l.logStarlark), nil

	default:
		return nil, nil
	}
}

func (l *CSVLog) logStarlark(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	strings := make([]string, len(args)+1)
	strings[0] = time.Now().Format("2006-01-02 15:04:05.000000")

	for i, v := range args {
		if s, ok := v.(starlark.String); ok {
			strings[i+1] = string(s)
		} else {
			strings[i+1] = v.String()
		}
	}

	l.Log(strings)
	return starlark.None, nil
}

func customCSVLog(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackPositionalArgs(fn.Name(), args, kwargs, 1, &path); err != nil {
		return nil, err
	}

	customLogLock.Lock()
	defer customLogLock.Unlock()

	l, ok := customLogs[path]
	if ok {
		return l, nil
	}

	l = new(CSVLog)
	l.Open(path)
	customLogs[path] = l
	return l, nil
}
