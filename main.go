package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Port              int
	IP                string
	IPwhitelist       bool
	WhitelistedIPs    map[string]bool
	CsvPath           string
	LogPath           string
	StaticCommand     string
	ReturnResult      bool
	RateLimit         int
	ReplaceParam      bool
	ReplaceRegex      *regexp.Regexp
	DontStopReplacing bool
	Args              []string //old input
}

func LoadRoutesIntoMap(newMap *sync.Map, csvText []byte, logger *slog.Logger) error {

	r := csv.NewReader(bytes.NewReader(csvText))
	rows, err := r.ReadAll()
	if err != nil {
		logger.Error("Error parsing csv!")
		return err
	}

	for k, row := range rows {
		path := row[0]
		if path == "" {
			logger.Warn("Skipping row because of missing URL", "row", k+1)
			continue
		}
		cmd := row[1]
		if cmd == "" {
			logger.Warn("Skipping row because of missing command", "row", k+1)
			continue
		}
		newMap.Store(path, cmd)
	}
	return nil
}

func ParseIPList(input string) map[string]bool {
	newMap := map[string]bool{}
	list := strings.Split(input, ",")
	for _, ip := range list {
		newMap[strings.TrimSpace(ip)] = true
	}
	return newMap
}

func parseFlags(appname string, args []string) (config *Config, output string, err error) {
	flags := flag.NewFlagSet(appname, flag.ContinueOnError)
	var buf bytes.Buffer
	flags.SetOutput(&buf)

	var conf Config
	var allowedIPs string
	var regex string
	flags.IntVar(&conf.Port, "port", 4870, "port to listen on")
	flags.StringVar(&conf.IP, "ip", "127.0.0.1", "which ip to listen on")
	flags.StringVar(&allowedIPs, "allowedips", "127.0.0.1", "which ips to respond to in a comma-sep list, e.g. `1.1.1.1,3.3.3.3` (set to \"\" to disable)")
	flags.StringVar(&conf.CsvPath, "routes", "", "bash commands file to load, e.g. `./routes.csv`")
	flags.StringVar(&conf.LogPath, "log", "stdout", "where to log to, e.g. `./spuria.log`")
	flags.StringVar(&conf.StaticCommand, "cmd", "", "static command to execute for /do , e.g. `\"echo 'hi'\"` , if this is set no csv (-routes) will be loaded")
	flags.BoolVar(&conf.ReturnResult, "returnresult", false, "returns the command output in the http response, default is OK/ERR for 200/500 response body")
	flags.IntVar(&conf.RateLimit, "maxratelimit", 10, "requests allowed per URL per minute, 0 = infinite")
	flags.BoolVar(&conf.ReplaceParam, "replaceparam", false, "replace GET parameters starting with $ inside the bash script")
	flags.StringVar(&regex, "replaceregex", "^[ a-zA-Z0-9/-]*$", "regex for allowed GET parameter replacing characters")
	flags.BoolVar(&conf.DontStopReplacing, "nostop", false, "do not stop when encountering an error in the GET parameter replacement")

	err = flags.Parse(args)
	if err != nil {
		return nil, buf.String(), err
	}

	conf.WhitelistedIPs = map[string]bool{}
	if allowedIPs != "" {
		conf.IPwhitelist = true
		conf.WhitelistedIPs = ParseIPList(allowedIPs)
	}

	// fmt.Println("regex:",regex)
	conf.ReplaceRegex, err = regexp.Compile(regex)
	if err != nil {
		return nil, buf.String(), err
	}

	conf.Args = flags.Args()
	return &conf, buf.String(), nil
}

func main() {
	starttime := time.Now()

	config, output, err := parseFlags(os.Args[0], os.Args[1:])
	if err == flag.ErrHelp {
		fmt.Println(output)
		os.Exit(2)
	} else if err != nil {
		fmt.Println("ERROR:", err)
		// fmt.Println("FLAG OUTPUT:",output)
		os.Exit(1)
	}

	//log
	var logger *slog.Logger
	if config.LogPath == "stdout" {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	} else {
		f, err := os.OpenFile(config.LogPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			fmt.Println("ERROR opening log file")
			panic(err)
		}
		defer f.Close()
		logger = slog.New(slog.NewTextHandler(f, nil))
	}

	//routes / bash commands
	funcMap := sync.Map{}
	if config.StaticCommand != "" {
		funcMap.Store("/do", config.StaticCommand)
	} else if config.CsvPath != "" {
		fileBytes, err := os.ReadFile(config.CsvPath)
		if err != nil {
			logger.Error("Couldn't read config.csv!")
			panic(err)
		}
		err = LoadRoutesIntoMap(&funcMap, fileBytes, logger)
		if err != nil {
			panic(err)
		}
	} else {
		panic("ERROR: Please provide either -routes or -cmd !")
	}

	httpServer := &http.Server{
		Addr:    net.JoinHostPort(config.IP, strconv.Itoa(config.Port)),
		Handler: NewServer(config, &funcMap, logger),
	}

	donetime := time.Now()
	logger.Info("Startup finished", "timetaken", donetime.Sub(starttime).String(), "ip", config.IP, "port", config.Port, "configLocation", config.CsvPath, "allowedIPs", config.WhitelistedIPs, "logLocation", config.LogPath)
	fmt.Println("Listening on ", config.IP, ":", config.Port)

	fmt.Println(httpServer.ListenAndServe())
}

func NewServer(config *Config, funcMap *sync.Map, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	//ratelimit
	mu := sync.Mutex{}
	// _=mu
	reqCounter := map[string]int{}
	// _=reqCounter
	resetTime := atomic.Int64{}
	resetTime.Store(time.Now().Add(60 * time.Second).Unix())

	//web
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "# TYPE isupdummy counter")
		fmt.Fprintln(w, "isupdummy 1")
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			LogRequest(logger, r, 200, nil)
			return
		}

		defer func() {
			if rc := recover(); rc != nil {
				err := rc.(error)
				LogRequest(logger, r, 500, err)
			}
		}()

		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			fmt.Println("ERROR WHEN PARSING REMOTEADDR")
			fmt.Println(err)
			return
		}

		//ip whitelist
		if exists, value := config.WhitelistedIPs[ip]; config.IPwhitelist && (!exists || !value) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "NOACCESS")
			LogRequest(logger, r, 403, nil)
			return
		}

		//ratelimiter
		mu.Lock()
		reqCounter[r.URL.Path]++
		timeWhenReset := resetTime.Load()
		if time.Now().Unix() > timeWhenReset {
			reqCounter[r.URL.Path] = 1
			resetTime.Store(time.Now().Add(60 * time.Second).Unix())
		}
		if reqCounter[r.URL.Path] > config.RateLimit && config.RateLimit != 0 {
			mu.Unlock()
			w.WriteHeader(http.StatusTooManyRequests)
			LogRequest(logger, r, 429, nil)
			return
		}
		mu.Unlock()

		if value, exists := funcMap.Load(r.URL.Path); exists {
			err, stdout, stderr := ExecuteCommand(r, value.(string), config, logger)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				if config.ReturnResult {
					fmt.Fprint(w, stderr)
				} else {
					fmt.Fprint(w, "ERR")
				}
				LogRequest(logger, r, 500, nil)
			} else {
				w.WriteHeader(http.StatusOK)
				if config.ReturnResult {
					fmt.Fprint(w, stdout)
				} else {
					fmt.Fprint(w, "OK")
				}
				LogRequest(logger, r, 200, nil)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "URL not found or configured! (%q)", r.URL.Path)
		LogRequest(logger, r, 404, nil)
	})
	return mux
}

func LogRequest(logger *slog.Logger, r *http.Request, returnCode int, err error) {
	logger.Info("request", "method", r.Method, "url", r.URL.Path, "status", returnCode, "source", r.RemoteAddr, "proto", r.Proto, "host", r.Host, "referer", r.Referer(), "useragent", r.UserAgent(), "err", err)
}

func ExecuteCommand(r *http.Request, command string, config *Config, logger *slog.Logger) (error, string, string) {
	path := r.URL.Path
	params := r.URL.Query()
	if len(params) > 0 && config.ReplaceParam {
		logger.Info("replacing params", "params", params)
		for name, values := range params {
			value := values[0]
			if len(values) <= 0 || len(values) > 1 {
				logger.Warn("get param error, please only set GET parameter value once for each key", "path", path, "name", name, "length", len(values))
				if config.DontStopReplacing {
					continue
				} else {
					return errors.New("GET param has more than 1 or less than 1 values"), "", ""
				}
			}

			if !strings.HasPrefix(name, "$") {
				logger.Warn("get param error, name has to begin with $", "path", path, "name", name, "value", value)
				if config.DontStopReplacing {
					continue
				} else {
					return errors.New("GET param name doesn't begin with $"), "", ""
				}
			}
			if !config.ReplaceRegex.MatchString(value) {
				logger.Warn("get param error, invalid input for regex", "path", path, "name", name, "value", value)
				if config.DontStopReplacing {
					continue
				} else {
					return errors.New("GET param value doesn't match regex"), "", ""
				}
			}
			command = strings.ReplaceAll(command, name, value)
		}
	}
	ec := exec.Command("bash", "-c", command) //.Output()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	ec.Stdout = &stdout
	ec.Stderr = &stderr
	starttime := time.Now()
	err := ec.Run()
	timeTaken := time.Since(starttime).String()
	outStr := stdout.String()
	errStr := stderr.String()
	if err != nil {
		logger.Error("execution error", "path", path, "duration", timeTaken, "stdout", outStr, "stderr", errStr, "err", err)
		return err, outStr, errStr
	}
	logger.Info("execution success", "path", path, "duration", timeTaken, "stdout", outStr, "stderr", errStr)
	return nil, outStr, errStr
}
