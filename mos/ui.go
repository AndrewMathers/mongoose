package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/context"

	"cesanta.com/common/go/ourutil"
	"cesanta.com/mos/build"
	"cesanta.com/mos/dev"
	"cesanta.com/mos/update"
	"cesanta.com/mos/version"
	"github.com/cesanta/errors"
	"github.com/elazarl/go-bindata-assetfs"
	"github.com/golang/glog"
	shellwords "github.com/mattn/go-shellwords"
	"github.com/skratchdot/open-golang/open"
	flag "github.com/spf13/pflag"

	"golang.org/x/net/websocket"
)

const (
	expireTime = 1 * time.Minute

	// Show webview on windows starting from Windows 8
	// (see windows versions info here: https://msdn.microsoft.com/en-us/library/windows/desktop/ms724832(v=vs.85).aspx)
	webviewMinWindowsVersion = "6.2"
)

type projectType string

const (
	projectTypeApp projectType = "app"
	projectTypeLib projectType = "lib"
)

var (
	httpAddr     = "127.0.0.1:1992"
	udpAddr      = ":1993"
	wsClients    = make(map[*websocket.Conn]int)
	wsClientsMtx = sync.Mutex{}
	wwwRoot      = ""
	startBrowser = true
	startWebview = false
	inLogLine    = map[string]bool{}
	devConnMtx   = sync.Mutex{}

	origStdout = os.Stdout
	origStderr = os.Stderr
)

type wsmessage struct {
	Cmd  string `json:"cmd"`
	Data string `json:"data"`
}

// Return result of the /list-apps and /list-libs endpoints
type appLibList map[string]*build.FWAppManifest

type saveAppLibFileParams struct {
	// base64-encoded data
	Data string `yaml:"data"`
}

func wsSend(ws *websocket.Conn, m wsmessage) {
	message := ""
	for i, b := range m.Data {
		if !inLogLine[m.Cmd] {
			message += FormatTimestampNow()
		}
		message += string(m.Data[i : i+1])
		inLogLine[m.Cmd] = (b != '\n')
	}
	m.Data = message
	t, _ := json.Marshal(m)
	websocket.Message.Send(ws, string(t))
}

func wsBroadcast(m wsmessage) {
	wsClientsMtx.Lock()
	defer wsClientsMtx.Unlock()
	for ws := range wsClients {
		wsSend(ws, m)
	}
}

type errmessage struct {
	Error string `json:"error"`
}

func wsHandler(ws *websocket.Conn) {
	defer func() {
		wsClientsMtx.Lock()
		defer wsClientsMtx.Unlock()
		delete(wsClients, ws)
		ws.Close()
	}()
	wsClientsMtx.Lock()
	wsClients[ws] = 1
	wsClientsMtx.Unlock()

	for {
		var text string
		err := websocket.Message.Receive(ws, &text)
		if err != nil {
			glog.Infof("Websocket recv error: %v, closing connection", err)
			return
		}
	}
}

func reportConsoleLogs() {
	for {
		data := <-consoleMsgs
		wsBroadcast(wsmessage{"uart", string(data)})
	}
}

func httpReplyExt(w http.ResponseWriter, result interface{}, err error, asJSON bool) {
	var msg []byte
	if err != nil {
		msg, _ = json.Marshal(errmessage{err.Error()})
	} else {
		if asJSON {
			s := result.(string)
			msg = []byte(fmt.Sprintf(`{"result": %s}`, s))
		} else {
			r := map[string]interface{}{
				"result": result,
			}
			msg, err = json.Marshal(r)
		}
	}

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, string(msg))
	} else {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, string(msg))
	}
}

func httpReply(w http.ResponseWriter, result interface{}, err error) {
	s, ok := result.(string)
	asJSON := ok && isJSON(s)
	httpReplyExt(w, result, err, asJSON)
}

func init() {
	// TODO: fix webview issues https://www.youtube.com/watch?v=5xFkZYfyP5A and
	// uncomment

	//switch runtime.GOOS {
	//case "linux":
	//// leave false
	//case "darwin":
	//startWebview = true
	//case "windows":
	//// On windows, open webview only at Windows 8 and later
	//windowsVersion := goInfo.GetInfo().Core
	//if goversion.CompareNormalized(windowsVersion, webviewMinWindowsVersion, ">=") {
	//startWebview = true
	//}
	//}

	flag.StringVar(&wwwRoot, "web-root", "", "UI Web root to use instead of built-in")
	hiddenFlags = append(hiddenFlags, "web-root")

	flag.StringVar(&httpAddr, "http-addr", "127.0.0.1:1992", "Web UI HTTP address")
	hiddenFlags = append(hiddenFlags, "http-addr")

	flag.BoolVar(&startBrowser, "start-browser", true, "Automatically start browser")
	hiddenFlags = append(hiddenFlags, "start-browser")

	flag.BoolVar(&startWebview, "start-webview", startWebview, "Automatically start WebView")
	hiddenFlags = append(hiddenFlags, "start-webview")
}

func devlock(devConn *dev.DevConn) {
	if devConn == nil {
		glog.Infof("Locking device NIL")
	} else {
		glog.Infof("Locking device %v", devConn.IsConnected())
	}
	devConnMtx.Lock()
	glog.Infof("Locked.")
}

func devunlock() {
	glog.Infof("Unlocking device")
	devConnMtx.Unlock()
}

func reconnectToDevice(ctx context.Context) (*dev.DevConn, error) {
	return createDevConnWithJunkHandler(ctx, consoleJunkHandler, MqttLogHandler)
}

func MqttLogHandler(topic string, data []byte) {
	wsBroadcast(wsmessage{"uart", string(data)})
}

func UDPLogCatcher() {
	addr, err := net.ResolveUDPAddr("udp", udpAddr)
	if err != nil {
		fmt.Println("Error: ", err)
		return
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Println("Error: ", err)
		return
	}
	defer conn.Close()
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Println("Error: ", err)
			wsBroadcast(wsmessage{"uart", fmt.Sprintf("Error: %v", err)})
		} else {
			wsBroadcast(wsmessage{"uart", string(buf[:n])})
		}
	}
}

func startUI(ctx context.Context, devConn *dev.DevConn) error {
	glog.CopyStandardLogTo("INFO")
	go reportConsoleLogs()
	go UDPLogCatcher()
	http.Handle("/ws", websocket.Handler(wsHandler))

	r, w, _ := os.Pipe()
	os.Stdout = w
	os.Stderr = w
	go func() {
		for {
			data := make([]byte, 512)
			n, err := r.Read(data)
			if err != nil {
				break
			}
			wsBroadcast(wsmessage{"stderr", string(data[:n])})
		}
	}()

	http.HandleFunc("/flash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		r.ParseForm()
		*firmware = r.FormValue("firmware")

		// TODO(lsm): the following snippet is similar to the one in "/terminal"
		// handler, refactor to reduce copypasta.
		devlock(devConn)
		defer devunlock()
		if devConn != nil {
			devConn.Disconnect(ctx)
			devConn = nil
		}
		defer func() {
			time.Sleep(time.Second)
			devConn, _ = reconnectToDevice(ctx)
		}()
		time.Sleep(time.Second) // Close really really
		ctx2, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()

		os.Args = []string{
			"flash", "--port", *portFlag, "--firmware", *firmware,
			"--v", "4", "--logtostderr",
		}
		err := flash(ctx2, nil)
		httpReply(w, true, err)
	})

	http.HandleFunc("/wifi", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		args := []string{
			"wifi.ap.enable=false",
			"wifi.sta.enable=true",
			fmt.Sprintf("wifi.sta.ssid=%s", r.FormValue("ssid")),
			fmt.Sprintf("wifi.sta.pass=%s", r.FormValue("pass")),
		}
		// We need 15-seconds timeout in order for the bad password to be detected
		// properly. Previously we had 10 seconds, and the context was timing out
		// before the bad password result was returned.
		ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		devlock(devConn)
		defer devunlock()

		if devConn == nil {
			httpReply(w, nil, errors.Errorf("Device is not connected"))
			return
		}

		err := internalConfigSet(ctx2, devConn, args)
		result := "false"
		if err == nil {
			for {
				time.Sleep(time.Millisecond * 500)
				res2, err := devConn.GetInfo(ctx2)
				if err != nil {
					httpReply(w, result, err)
					return
				}
				wifiStatus := *res2.Wifi.Status
				if wifiStatus == "got ip" {
					result = fmt.Sprintf(`"%s"`, *res2.Wifi.Sta_ip)
					break
				} else if wifiStatus == "connecting" || wifiStatus == "connected" || wifiStatus == "" || wifiStatus == "associated" {
					// Still connecting, wait
				} else {
					err = errors.Errorf("%s", wifiStatus)
					break
				}
			}
		}
		httpReply(w, result, err)
	})

	http.HandleFunc("/policies", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		awsRegion = r.FormValue("region")
		arr, err := getAWSIoTPolicyNames()
		if err == nil {
			sort.Strings(arr)
			// Include the default policy, even if not present - it will be created.
			if sort.SearchStrings(arr, awsIoTPolicyMOS) >= len(arr) {
				arr = append(arr, awsIoTPolicyMOS)
				sort.Strings(arr)
			}
		}
		httpReply(w, arr, err)
	})

	http.HandleFunc("/regions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		httpReply(w, getAWSRegions(), nil)
	})

	http.HandleFunc("/connect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		portArg := r.FormValue("port")
		reconnect := r.FormValue("reconnect")

		devlock(devConn)
		defer devunlock()

		// If we're already connected to the given port, and the caller didn't
		// explicitly ask to reconnect in any case, don't do anything and just
		// report success
		if portArg == *portFlag && devConn != nil && reconnect == "" {
			httpReply(w, true, nil)
			return
		}

		if devConn != nil {
			devConn.Disconnect(ctx)
			devConn = nil
		}
		if portArg != "" {
			*portFlag = portArg
		}
		var err error
		devConn, err = reconnectToDevice(ctx)
		httpReply(w, true, err)
	})

	http.HandleFunc("/version-tag", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		httpReplyExt(w, version.GetMosVersion(), nil, false /* not as JSON */)
	})

	http.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		httpReply(w, version.BuildId, nil)
	})

	http.HandleFunc("/server-version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if version.LooksLikeDistrBuildId(version.BuildId) {
			// In distro-built mos, don't check server-version because the update is
			// done via other means
			httpReply(w, false, nil)
			return
		}

		v, err := update.GetServerMosVersion(update.GetUpdateChannel())
		if err != nil {
			httpReply(w, false, err)
			return
		}

		httpReply(w, v.BuildId, nil)
	})

	http.HandleFunc("/put", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		data := r.FormValue("data")
		path := r.FormValue("path")
		if path == "" {
			httpReply(w, false, errors.Errorf("path is not specified"))
			return
		}

		ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		devlock(devConn)
		defer devunlock()

		if devConn == nil {
			httpReply(w, nil, errors.Errorf("Device is not connected"))
			return
		}

		err := fsPutData(ctx2, devConn, bytes.NewReader([]byte(data)), path)
		httpReply(w, err == nil, err)
	})

	http.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		devlock(devConn)
		defer devunlock()

		if devConn == nil {
			httpReply(w, nil, errors.Errorf("Device is not connected"))
			return
		}

		text, err := getFile(ctx2, devConn, r.FormValue("name"))
		if err == nil {
			text2, err2 := json.Marshal(text)
			if err2 == nil {
				text = string(text2)
			} else {
				err = err2
			}
		}
		httpReply(w, text, err)
	})

	http.HandleFunc("/aws-iot-setup", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		awsIoTPolicy = r.FormValue("policy")
		awsRegion = r.FormValue("region")

		ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		devlock(devConn)
		defer devunlock()

		err := awsIoTSetup(ctx2, devConn)
		httpReply(w, true, err)
	})

	http.HandleFunc("/aws-store-creds", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := storeCreds(r.FormValue("key"), r.FormValue("secret"))
		httpReply(w, true, err)
	})

	http.HandleFunc("/check-aws-credentials", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := getAwsCredentials()
		httpReply(w, err == nil, nil)
	})

	http.HandleFunc("/getports", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		type GetPortsResult struct {
			IsConnected bool
			CurrentPort string
			PortFlag    string
			Ports       []string
		}
		reply := GetPortsResult{false, "", *portFlag, enumerateSerialPorts()}
		devlock(devConn)
		if devConn != nil {
			reply.CurrentPort = devConn.ConnectAddr
			reply.IsConnected = devConn.IsConnected()
		}
		devunlock()
		glog.Infof("enumerated, returning...")

		httpReply(w, reply, nil)
	})

	http.HandleFunc("/list_aws_things", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		reply, err := getAWSIoTThings()
		httpReply(w, reply, err)
	})

	http.HandleFunc("/infolog", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		glog.Flush()
		pattern := fmt.Sprintf("%s/mos*INFO*.%d", os.TempDir(), os.Getpid())
		paths, err := filepath.Glob(pattern)
		if err == nil && len(paths) > 0 {
			http.ServeFile(w, r, paths[0])
		}
	})

	http.HandleFunc("/call", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		method := r.FormValue("method")

		if method == "" {
			httpReply(w, nil, errors.Errorf("Expecting method"))
			return
		}
		args := r.FormValue("args")
		glog.Errorf("Calling: %+v", method)
		glog.Infof("Calling: %+v, args: %+v", method, args)

		timeout, err2 := strconv.ParseInt(r.FormValue("timeout"), 10, 64)
		if err2 != nil {
			timeout = 10
		}
		ctx2, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()

		devlock(devConn)
		defer devunlock()

		if devConn == nil {
			httpReply(w, nil, errors.Errorf("Device is not connected"))
			return
		}

		result, err := callDeviceService(ctx2, devConn, method, args)
		if method == "Config.Save" {
			// Saving config causes the device to reboot, so we have to wait a bit
			waitForReboot()
		}
		if err != nil {
			glog.Errorf("Call complete, error: %v", err)
		} else {
			glog.Errorf("Call complete, success")
		}
		glog.Infof("Call result: %+v, error: %+v", result, err)
		httpReply(w, result, err)
	})

	http.HandleFunc("/terminal", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Get the command line value, modify os.Args and re-parse flags
		str := r.FormValue("cmd")
		args, err := shellwords.Parse(str)
		if err != nil {
			httpReply(w, true, err)
			return
		}
		if len(args) > 0 && args[0] != "mos" {
			args = append([]string{"mos"}, args...)
		}
		os.Args = args
		if len(os.Args) > 1 && os.Args[1] == "-X" {
			os.Args = append(os.Args[:1], os.Args[2:]...)
			extendedMode = true
			commands = append(commands, extendedCommands...)
		}
		flag.CommandLine.Init("mos", flag.ContinueOnError)
		flag.Parse()

		// Some commands want special access to the serial port,
		// therefore close device connection here and schedule the reconnection
		// after this function exits.
		cmd := getCommand(flag.Arg(0))

		if cmd != nil && !cmd.needDevConn {
			devlock(devConn)
			defer devunlock()

			// On MacOS and Windows, sleep for 1 second after we close serial
			// port. Otherwise, open call fails for some reason we have
			// no idea about. Thus those time.Sleep() calls below.
			if devConn != nil {
				devConn.Disconnect(ctx)
				devConn = nil
			}
			defer func() {
				time.Sleep(time.Second)
				devConn, _ = reconnectToDevice(ctx)
			}()
			time.Sleep(time.Second)
		}

		ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		err = run(cmd, ctx2, devConn)
		httpReply(w, true, err)
	})

	initProjectManagementEndpoints()

	http.HandleFunc("/update", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		result := true

		err := update.Update(ctx, devConn)
		if err != nil {
			err = errors.Trace(err)
			result = false
		}

		httpReply(w, result, err)
	})

	if wwwRoot != "" {
		http.HandleFunc("/", addNoCacheHeader(http.FileServer(http.Dir(wwwRoot))))
	} else {
		assetInfo := func(path string) (os.FileInfo, error) {
			return os.Stat(path)
		}
		http.Handle("/", addNoCacheHeader(http.FileServer(&assetfs.AssetFS{Asset: Asset,
			AssetDir: AssetDir, AssetInfo: assetInfo, Prefix: "web_root"})))
	}
	url := fmt.Sprintf("http://%s", httpAddr)

	ourutil.Reportf("To get a list of available commands, start with --help")
	ourutil.Reportf("Starting Web UI. If the browser does not start, navigate to %s", url)
	listener, err := net.Listen("tcp", httpAddr)
	if err != nil {
		os.Stdout = origStdout
		os.Stderr = origStderr
		return errors.Trace(err)
	}
	if startWebview {
		go http.Serve(listener, nil)
		webview(url)
	} else {
		if startBrowser {
			open.Start(url)
		}
		http.Serve(listener, nil)
	}

	// Unreacahble
	return nil
}

func addNoCacheHeader(handler http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		handler.ServeHTTP(w, r)
	}
}
