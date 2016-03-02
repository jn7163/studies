// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/CodisLabs/codis/pkg/proxy"
	"github.com/CodisLabs/codis/pkg/proxy/router"
	"github.com/CodisLabs/codis/pkg/utils"
	"github.com/CodisLabs/codis/pkg/utils/bytesize"
	"github.com/CodisLabs/codis/pkg/utils/log"
	"github.com/docopt/docopt-go"
	"github.com/ngaut/gostats"
)

var (
	cpus       = 2
	addr       = ":9000"
	httpAddr   = ":9001"
	configFile = "config.ini"
)

var usage = `usage: proxy [-c <config_file>] [-L <log_file>] [--log-level=<loglevel>] [--log-filesize=<filesize>] [--cpu=<cpu_num>] [--addr=<proxy_listen_addr>] [--http-addr=<debug_http_server_addr>]

options:
   -c	set config file
   -L	set output log file, default is stdout
   --log-level=<loglevel>	set log level: info, warn, error, debug [default: info]
   --log-filesize=<maxsize>  set max log file size, suffixes "KB", "MB", "GB" are allowed, 1KB=1024 bytes, etc. Default is 1GB.
   --cpu=<cpu_num>		num of cpu cores that proxy can use
   --addr=<proxy_listen_addr>		proxy listen address, example: 0.0.0.0:9000
   --http-addr=<debug_http_server_addr>		debug vars http server
`

const banner string = `
  _____  ____    ____/ /  (_)  _____
 / ___/ / __ \  / __  /  / /  / ___/
/ /__  / /_/ / / /_/ /  / /  (__  )
\___/  \____/  \__,_/  /_/  /____/

`

// 初始化
func init() {
	log.SetLevel(log.LEVEL_INFO)
}

func setLogLevel(level string) {
	level = strings.ToLower(level)
	var l = log.LEVEL_INFO
	switch level {
	case "error":
		l = log.LEVEL_ERROR
	case "warn", "warning":
		l = log.LEVEL_WARN
	case "debug":
		l = log.LEVEL_DEBUG
	case "info":
		fallthrough
	default:
		level = "info"
		l = log.LEVEL_INFO
	}
	log.SetLevel(l)
	log.Infof("set log level to <%s>", level)
}

func setCrashLog(file string) {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.InfoErrorf(err, "cannot open crash log file: %s", file)
	} else {
		syscall.Dup2(int(f.Fd()), 2)
	}
}

// 通过http接口动态设置日志级别
func handleSetLogLevel(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	setLogLevel(r.Form.Get("level"))
}

// 检查 ulimit -n 是否大于min
func checkUlimit(min int) {
	ulimitN, err := exec.Command("/bin/sh", "-c", "ulimit -n").Output()
	if err != nil {
		log.WarnErrorf(err, "get ulimit failed")
	}

	n, err := strconv.Atoi(strings.TrimSpace(string(ulimitN)))
	if err != nil || n < min {
		log.Panicf("ulimit too small: %d, should be at least %d", n, min)
	}
}

func main() {
	args, err := docopt.Parse(usage, nil, true, utils.Version, true)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Print(banner)

	// set config file
	if args["-c"] != nil {
		configFile = args["-c"].(string)
	}

	// 设置日志文件大小最大值
	var maxFileFrag = 10000000
	var maxFragSize int64 = bytesize.GB * 1
	if s, ok := args["--log-filesize"].(string); ok && s != "" {
		v, err := bytesize.Parse(s)
		if err != nil {
			log.PanicErrorf(err, "invalid max log file size = %s", s)
		}
		maxFragSize = v
	}

	// set output log file
	if s, ok := args["-L"].(string); ok && s != "" {
		f, err := log.NewRollingFile(s, maxFileFrag, maxFragSize)
		if err != nil {
			log.PanicErrorf(err, "open rolling log file failed: %s", s)
		} else {
			defer f.Close()
			log.StdLog = log.New(f, "")
		}
	}
	log.SetLevel(log.LEVEL_INFO)
	log.SetFlags(log.Flags() | log.Lshortfile)

	// set log level
	if s, ok := args["--log-level"].(string); ok && s != "" {
		setLogLevel(s)
	}

	cpus = runtime.NumCPU()
	// set cpu
	// 设置使用cpu核数
	if args["--cpu"] != nil {
		cpus, err = strconv.Atoi(args["--cpu"].(string))
		if err != nil {
			log.PanicErrorf(err, "parse cpu number failed")
		}
	}

	// set addr
	// 设置作为代理的绑定的地址
	if args["--addr"] != nil {
		addr = args["--addr"].(string)
	}

	// set http addr
	// 设置提供http接口绑定的地址
	if args["--http-addr"] != nil {
		httpAddr = args["--http-addr"].(string)
	}

	// 可打开文件描述符数必须大于1024
	checkUlimit(1024)
	// 设置go使用的cpu数
	runtime.GOMAXPROCS(cpus)

	// 可通过http请求动态调整日志级别
	http.HandleFunc("/setloglevel", handleSetLogLevel)
	go func() {
		err := http.ListenAndServe(httpAddr, nil)
		log.PanicError(err, "http debug server quit")
	}()
	log.Info("running on ", addr)

	// 加载配置文件
	conf, err := proxy.LoadConf(configFile)
	if err != nil {
		log.PanicErrorf(err, "load config failed")
	}

	// 捕获 SIGTERM 信号
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, os.Kill)

	// 建立一个新的proxy-server，会开启相关协程处理 redis-client 的请求，和后端 redis-server 建立连接
	// 是主要的逻辑处理部分
	s := proxy.New(addr, httpAddr, conf)
	defer s.Close()

	// stats包 提供了一个http接口获取相关信息  /debug/vars
	stats.PublishJSONFunc("router", func() string {
		var m = make(map[string]interface{})
		m["ops"] = router.OpCounts()
		m["cmds"] = router.GetAllOpStats()
		m["info"] = s.Info()
		m["build"] = map[string]interface{}{
			"version": utils.Version,
			"compile": utils.Compile,
		}
		b, _ := json.Marshal(m)
		return string(b)
	})

	go func() {
		<-c
		log.Info("ctrl-c or SIGTERM found, bye bye...")
		s.Close()
	}()

	// 等待1秒后将自己的状态设置为online
	time.Sleep(time.Second)
	if err := s.SetMyselfOnline(); err != nil {
		log.WarnError(err, "mark myself online fail, you need mark online manually by dashboard")
	}

	// 等待 proxy_server 退出
	s.Join()
	log.Infof("proxy exit!! :(")
}