package bot

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fatalbanana/bananaboatbot/client"
	"github.com/yuin/gopher-lua"
	"golang.org/x/net/html"
	irc "gopkg.in/sorcix/irc.v2"
)

// BananaBoatBot contains config & state of the bot
type BananaBoatBot struct {
	// Config contains elements that are passed on initialization
	Config *BananaBoatBotConfig
	// curNet is set to friendly name of network we're handling a message from
	curNet string
	// curMessage is set to the message being handled
	curMessage *irc.Message
	// handlers is a map of IRC command names to Lua functions
	handlers map[string]*lua.LFunction
	// handlersMutex protects the handlers map
	handlersMutex sync.RWMutex
	// httpClient is used for HTTP requests
	httpClient http.Client
	// luaMutex protects shared Lua state
	luaMutex sync.Mutex
	// luaPool is a pool for when shared state is undesirable
	luaPool sync.Pool
	// luaState contains shared Lua state
	luaState *lua.LState
	// nick is the default nick of the bot
	nick string
	// realname is the default "real name" of the bot
	realname string
	// username is the default username of the bot
	username string
	// servers is a map of friendly names to IRC servers
	Servers sync.Map
	// mutex for handling of servers
	serversMutex sync.Mutex
}

// Close handles shutdown-related tasks
func (b *BananaBoatBot) Close(ctx context.Context) {
	log.Print("Shutting down")
	b.Servers.Range(func(k, value interface{}) bool {
		value.(client.IrcServerInterface).Close(ctx)
		return true
	})
	b.luaMutex.Lock()
	b.luaState.Close()
	b.luaMutex.Unlock()
}

func luaParamsFromMessage(svrName string, msg *irc.Message) []lua.LValue {
	// Make empty list of parameters to pass to Lua
	luaParams := make([]lua.LValue, len(msg.Params)+4)
	// First parameter is the name of the server
	luaParams[0] = lua.LString(svrName)
	// Following three parameters are nick/user/host if set
	if msg.Prefix != nil {
		luaParams[1] = lua.LString(msg.Prefix.Name)
		luaParams[2] = lua.LString(msg.Prefix.User)
		luaParams[3] = lua.LString(msg.Prefix.Host)
	}
	// Fifth parameter onwards is unpacked parameters of the irc.Message
	pi := 0
	for i := 4; i < len(luaParams); i++ {
		luaParams[i] = lua.LString(msg.Params[pi])
		pi++
	}
	return luaParams
}

func (b *BananaBoatBot) handleLuaReturnValues(ctx context.Context, svrName string, luaState *lua.LState) {
	// Ignore nil
	lv := luaState.Get(-1)
	if lv.Type() == lua.LTNil {
		return
	}
	// Get table result
	res := luaState.CheckTable(-1)
	// For each numeric index in the table result...
	res.ForEach(func(index lua.LValue, messageL lua.LValue) {
		var command string
		var params []string
		// Get the nested table..
		if message, ok := messageL.(*lua.LTable); ok {
			// Get 'command' string from table
			lv := message.RawGetString("command")
			command = lua.LVAsString(lv)
			// Get 'net' from table
			lv = message.RawGetString("net")
			net := lua.LVAsString(lv)
			// If missing use the current server
			if len(net) == 0 {
				net = svrName
			}
			// Get 'params' table from table
			lv = message.RawGetString("params")
			if paramsT, ok := lv.(*lua.LTable); ok {
				// Make a list of parameters
				params = make([]string, paramsT.MaxN())
				// Copy parameters from Lua
				paramsIndex := 0
				paramsT.ForEach(func(index lua.LValue, paramL lua.LValue) {
					params[paramsIndex] = lua.LVAsString(paramL)
					paramsIndex++
				})
			} else {
				// No parameters, make an empty array
				params = make([]string, 0)
			}
			// Create irc.Message
			ircMessage := &irc.Message{
				Command: command,
				Params:  params,
			}
			// Send it to the server
			svr, ok := b.Servers.Load(net)
			if ok {
				select {
				case svr.(client.IrcServerInterface).GetMessages() <- *ircMessage:
					break
				default:
					log.Printf("Channel full, message to server dropped: %s", ircMessage)
				}
			} else {
				log.Printf("Lua eror: Invalid server: %s", net)
			}
		}
	})
}

// HandleHandlers invokes any registered Lua handlers for a command
func (b *BananaBoatBot) HandleHandlers(ctx context.Context, svrName string, msg *irc.Message) {
	if b.Config.LogCommands {
		// Log message
		log.Printf("[%s] %s", svrName, msg)
	}
	// Get read mutex for handlers map
	b.handlersMutex.RLock()
	// If we have a function corresponding to this command...
	if luaFunction, ok := b.handlers[msg.Command]; ok {
		// Release read mutex for handlers
		b.handlersMutex.RUnlock()
		// Deferred release of lua state mutex
		defer b.luaMutex.Unlock()
		// Make list of parameters to pass to Lua
		luaParams := luaParamsFromMessage(svrName, msg)
		// Get Lua mutex
		b.luaMutex.Lock()
		// Store some state information
		b.curMessage = msg
		b.curNet = svrName
		// Call function
		err := b.luaState.CallByParam(lua.P{
			Fn:      luaFunction,
			NRet:    1,
			Protect: true,
		}, luaParams...)
		// Abort on failure
		if err != nil {
			log.Printf("Handler for %s failed: %s", msg.Command, err)
			return
		}
		// Handle return values
		b.handleLuaReturnValues(ctx, svrName, b.luaState)
		// Clear stack
		b.luaState.SetTop(0)
	} else {
		// Release read mutex for handlers
		b.handlersMutex.RUnlock()
	}
}

// ReconnectServers reconnects servers on error
func (b *BananaBoatBot) HandleErrors(ctx context.Context, svrName string, err error) {
	// Log the error
	log.Printf("[%s] Connection error: %s", svrName, err)

	b.serversMutex.Lock()

	// Try reconnect to the server if still configured
	svr, ok := b.Servers.Load(svrName)
	// Server is no longer configured, do nothing
	if !ok {
		b.serversMutex.Unlock()
		return
	}
	s := svr.(client.IrcServerInterface)
	// Error doesn't belong to current incarnation, do nothing
	if ctx.Done() != s.Done() {
		b.serversMutex.Unlock()
		return
	}
	s.Close(ctx)
	newSvr, svrCtx := b.Config.NewIrcServer(
		b.luaState.Context(),
		svrName,
		s.GetSettings())
	newSvr.SetReconnectExp(*(s.GetReconnectExp()))
	b.Servers.Store(svrName, newSvr)
	b.serversMutex.Unlock()
	newSvr.ReconnectWait(svrCtx)
	newSvr.Dial(svrCtx)
}

// ReloadLua deals with reloading Lua parts
func (b *BananaBoatBot) ReloadLua(ctx context.Context) error {
	b.luaMutex.Lock()
	defer func() {
		// Clear stack and release Lua mutex
		b.luaState.SetTop(0)
		b.luaMutex.Unlock()
	}()

	if err := b.luaState.DoFile(b.Config.LuaFile); err != nil {
		return err
	}

	lv := b.luaState.Get(-1)
	if lv.Type() != lua.LTTable {
		return fmt.Errorf("lua reload error: unexpected return type: %s", lv.Type())
	}
	tbl := lv.(*lua.LTable)

	lv = tbl.RawGetString("nick")
	nick := lua.LVAsString(lv)
	if len(nick) > 0 {
		b.nick = nick
	}

	lv = tbl.RawGetString("realname")
	realname := lua.LVAsString(lv)
	if len(realname) > 0 {
		b.realname = realname
	}

	lv = tbl.RawGetString("username")
	username := lua.LVAsString(lv)
	if len(username) > 0 {
		b.username = username
	}

	lv = tbl.RawGetString("handlers")
	defer b.handlersMutex.Unlock()
	b.handlersMutex.Lock()
	luaCommands := make(map[string]struct{})
	if handlerTbl, ok := lv.(*lua.LTable); ok {
		handlerTbl.ForEach(func(commandName lua.LValue, handlerFuncL lua.LValue) {
			if handlerFunc, ok := handlerFuncL.(*lua.LFunction); ok {
				commandNameStr := lua.LVAsString(commandName)
				b.handlers[commandNameStr] = handlerFunc
				luaCommands[commandNameStr] = struct{}{}
			}
		})
	} else {
		return fmt.Errorf("lua reload error: unexpected handlers type: %s", lv.Type())
	}

	// Delete handlers still in map but no longer defined in Lua
	for k := range b.handlers {
		if _, ok := luaCommands[k]; !ok {
			delete(b.handlers, k)
		}
	}

	// Make map of server names collected from Lua
	luaServerNames := make(map[string]struct{})
	// Get 'servers' from table
	lv = tbl.RawGetString("servers")
	// Get table value
	if serverTbl, ok := lv.(*lua.LTable); ok {
		// Iterate over nested tables...
		serverTbl.ForEach(func(serverName lua.LValue, serverSettingsLV lua.LValue) {
			// Get nested table
			if serverSettings, ok := serverSettingsLV.(*lua.LTable); ok {

				// Get 'server' string from table
				lv = serverSettings.RawGetString("server")
				host := lua.LVAsString(lv)

				// Get 'tls' bool from table (default false)
				var tls bool
				lv = serverSettings.RawGetString("tls")
				if lv, ok := lv.(lua.LBool); ok {
					tls = bool(lv)
				}

				// Get 'tls_verify' bool from table (default true)
				verifyTLS := true
				lv = serverSettings.RawGetString("tls_verify")
				if lv == lua.LFalse {
					verifyTLS = false
				}

				// Get 'port' from table (use default from so-called config)
				portInt := b.Config.DefaultIrcPort
				lv = serverSettings.RawGetString("port")
				if port, ok := lv.(lua.LNumber); ok {
					portInt = int(port)
				}

				// Get 'nick' from table - use default if unavailable
				var nick string
				lv = serverSettings.RawGetString("nick")
				if lv, ok := lv.(lua.LString); ok {
					nick = lua.LVAsString(lv)
				} else {
					nick = b.nick
				}

				// Get 'realname' from table - use default if unavailable
				var realname string
				lv = serverSettings.RawGetString("realname")
				if lv, ok := lv.(lua.LString); ok {
					realname = lua.LVAsString(lv)
				} else {
					realname = b.realname
				}

				// Get 'username' from table - use default if unavailable
				var username string
				lv = serverSettings.RawGetString("username")
				if lv, ok := lv.(lua.LString); ok {
					username = lua.LVAsString(lv)
				} else {
					username = b.username
				}

				// Remember we found this key
				serverNameStr := lua.LVAsString(serverName)
				luaServerNames[serverNameStr] = struct{}{}
				createServer := false
				serverSettings := &client.IrcServerSettings{
					Host:          host,
					Port:          portInt,
					TLS:           tls,
					VerifyTLS:     verifyTLS,
					Nick:          nick,
					MaxReconnect:  float64(b.Config.MaxReconnect),
					Realname:      realname,
					Username:      username,
					ErrorCallback: b.HandleErrors,
					InputCallback: b.HandleHandlers,
				}
				// Check if server already exists and/or if we need to (re)create it
				if oldSvr, ok := b.Servers.Load(serverNameStr); ok {
					oldSettings := oldSvr.(client.IrcServerInterface).GetSettings()
					if !(oldSettings.Host == serverSettings.Host &&
						oldSettings.Port == serverSettings.Port &&
						oldSettings.TLS == serverSettings.TLS &&
						oldSettings.VerifyTLS == serverSettings.VerifyTLS &&
						oldSettings.Nick == serverSettings.Nick &&
						oldSettings.Realname == serverSettings.Realname &&
						oldSettings.Username == serverSettings.Username) {
						createServer = true
					}
				} else {
					createServer = true
				}
				if createServer {
					log.Printf("Creating new IRC server: %s", serverNameStr)
					// Create new IRC server
					svr, svrCtx := b.Config.NewIrcServer(ctx, serverNameStr, serverSettings)
					// Set server to map
					oldSvr, ok := b.Servers.Load(serverNameStr)
					if ok {
						log.Printf("Destroying pre-existing IRC server: %s", serverNameStr)
						oldSvr.(client.IrcServerInterface).Close(ctx)
					}
					b.Servers.Store(serverNameStr, svr)
					go svr.(client.IrcServerInterface).Dial(svrCtx)
				}
			}
		})
	}

	// Remove servers no longer defined in Lua
	b.Servers.Range(func(k, value interface{}) bool {
		if _, ok := luaServerNames[k.(string)]; !ok {
			log.Printf("Destroying removed IRC server: %s", k)
			go value.(client.IrcServerInterface).Close(ctx)
			b.Servers.Delete(k)
		}
		return true
	})

	return nil
}

type OWMResponse struct {
	Conditions []OWMCondition `json:"weather"`
	Main       OWMMain        `json:"main"`
}

type OWMCondition struct {
	Description string `json:"description"`
}

type OWMMain struct {
	Temperature float64 `json:"temp"`
}

// luaLibOpenWeatherMap gets weather for a city
func (b *BananaBoatBot) luaLibOpenWeatherMap(luaState *lua.LState) int {
	apiKey := luaState.CheckString(1)
	location := luaState.CheckString(2)
	owmURL := fmt.Sprintf(b.Config.OwmURLTemplate, apiKey, location)
	resp, err := b.httpClient.Get(owmURL)
	if err != nil {
		log.Printf("HTTP client error: %s", err)
		return 0
	}
	if ct, ok := resp.Header["Content-Type"]; ok {
		if ct[0][:16] != "application/json" {
			log.Printf("OWM GET aborted: wrong content-type: %s", ct[0])
			return 0
		}
	} else {
		log.Print("OWM GET aborted: no content-type header")
		return 0
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("OWM GET returned non-OK status: %d", resp.StatusCode)
		return 0
	}
	dec := json.NewDecoder(resp.Body)
	owmResponse := &OWMResponse{}
	err = dec.Decode(&owmResponse)
	if err != nil {
		log.Printf("OWM response decode failed: %s", err)
		return 0
	}
	numConditions := len(owmResponse.Conditions) + 1
	conditions := make([]string, numConditions)
	conditions[0] = fmt.Sprintf("%.f°", owmResponse.Main.Temperature)
	i := 1
	for _, v := range owmResponse.Conditions {
		conditions[i] = v.Description
		i++
	}
	luaState.Push(lua.LString(strings.Join(conditions, ", ")))
	return 1
}

type LuisResponse struct {
	TopScoringIntent LuisTopScoringIntent `json:"topScoringIntent"`
	Entities         []LuisEntity         `json:"entities"`
}

type LuisTopScoringIntent struct {
	Intent string  `json:"intent"`
	Score  float64 `json:"score"`
}

type LuisEntity struct {
	Entity string  `json:"entity"`
	Type   string  `json:"type"`
	Score  float64 `json:"score"`
}

// luaLibLuisPredict predicts intention using luis.ai
func (b *BananaBoatBot) luaLibLuisPredict(luaState *lua.LState) int {
	region := luaState.CheckString(1)
	appID := luaState.CheckString(2)
	endpointKey := luaState.CheckString(3)
	utterance := luaState.CheckString(4)
	if len(utterance) > 500 {
		utterance = utterance[:500]
	}
	luisURL := fmt.Sprintf(b.Config.LuisURLTemplate, region, appID, endpointKey, url.QueryEscape(utterance))
	resp, err := b.httpClient.Get(luisURL)
	if err != nil {
		log.Printf("HTTP client error: %s", err)
		return 0
	}
	if ct, ok := resp.Header["Content-Type"]; ok {
		if ct[0][:16] != "application/json" {
			log.Printf("Luis GET aborted: wrong content-type: %s", ct[0])
			return 0
		}
	} else {
		log.Print("Luis GET aborted: no content-type header")
		return 0
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("Luis GET returned non-OK status: %d", resp.StatusCode)
		return 0
	}
	dec := json.NewDecoder(resp.Body)
	luisResponse := &LuisResponse{}
	err = dec.Decode(&luisResponse)
	if err != nil {
		log.Printf("Luis response decode failed: %s", err)
		return 0
	}
	if luisResponse.TopScoringIntent.Intent == "" {
		return 0
	}
	luaState.Push(lua.LString(luisResponse.TopScoringIntent.Intent))
	luaState.Push(lua.LNumber(luisResponse.TopScoringIntent.Score))
	entsTbl := luaState.CreateTable(0, 0)
	i := 1
	for _, e := range luisResponse.Entities {
		entTbl := luaState.CreateTable(0, 0)
		luaState.RawSet(entTbl, lua.LString("entity"), lua.LString(e.Entity))
		luaState.RawSet(entTbl, lua.LString("type"), lua.LString(e.Type))
		luaState.RawSet(entTbl, lua.LString("score"), lua.LNumber(e.Score))
		luaState.RawSetInt(entsTbl, i, entTbl)
		i++
	}
	luaState.Push(entsTbl)
	return 3
}

// luaLibRandom provides access to cryptographic random numbers in Lua
func (b *BananaBoatBot) luaLibRandom(luaState *lua.LState) int {
	// First argument should be int for upper bound (probably at least 1)
	i := luaState.ToInt(1)
	// Generate random integer given user supplied range
	r, err := rand.Int(rand.Reader, big.NewInt(int64(i)))
	// Add 1 to result
	res := r.Int64() + 1
	// Push result to stack
	ln := lua.LNumber(res)
	luaState.Push(ln)
	// Push error to stack
	if err == nil {
		// It might be nil in which case push nil
		luaState.Push(lua.LNil)
	} else {
		// Otherwise push the error text
		luaState.Push(lua.LString(err.Error()))
	}
	return 2
}

// luaLibWorker runs a task in a goroutine
func (b *BananaBoatBot) luaLibWorker(luaState *lua.LState) int {
	defer luaState.SetTop(0)
	// First parameter should be function
	functionProto := luaState.CheckFunction(1).Proto
	// Rest of parameters are parameters for that function
	numParams := luaState.GetTop()
	if numParams == 1 {
		numParams = 0
	} else {
		numParams--
	}
	luaParams := make([]lua.LValue, numParams)
	goIndex := 0
	for i := 2; goIndex < numParams; i++ {
		lv := luaState.Get(i)
		luaParams[goIndex] = lv
		goIndex++
	}
	// Run function in new goroutine
	go func(functionProto *lua.FunctionProto, curNet string, curMessage *irc.Message) {
		// Get luaState from pool
		newState := b.luaPool.Get().(*lua.LState)
		defer func() {
			// Clear stack and return state to pool
			newState.SetTop(0)
			b.luaPool.Put(newState)
		}()
		// Create function from prototype
		luaFunction := newState.NewFunctionFromProto(functionProto)
		// Sanitise parameters
		for i, v := range luaParams {
			if v.Type() == lua.LTFunction {
				luaParams[i] = newState.NewFunctionFromProto(v.(*lua.LFunction).Proto)
			}
		}
		// Call function
		err := newState.CallByParam(lua.P{
			Fn:      luaFunction,
			NRet:    1,
			Protect: true,
		}, luaParams...)
		if err != nil {
			log.Printf("worker: error calling Lua: %s", err)
			return
		}
		// Handle return values
		b.handleLuaReturnValues(newState.Context(), curNet, newState)
	}(functionProto, b.curNet, b.curMessage)
	return 0
}

// luaLibGetTitle tries to get the HTML title of a URL
func (b *BananaBoatBot) luaLibGetTitle(luaState *lua.LState) int {
	// First argument should be some URL to try process
	u := luaState.CheckString(1)
	// Make request
	resp, err := b.httpClient.Get(u)
	// Handle HTTP request failure
	if err != nil {
		luaState.Push(lua.LNil)
		log.Printf("HTTP client error: %s", err)
		return 1
	}
	// Expect to see text/html content-type
	if ct, ok := resp.Header["Content-Type"]; ok {
		if ct[0][:9] != "text/html" {
			luaState.Push(lua.LNil)
			log.Printf("GET of %s aborted: wrong content-type: %s", u, ct[0])
			return 1
		}
	} else {
		luaState.Push(lua.LNil)
		log.Printf("GET of %s aborted: no content-type header", u)
		return 1
	}
	// Read up to 12288 bytes
	limitedReader := &io.LimitedReader{R: resp.Body, N: 12288}
	// Create new tokenizer
	tokenizer := html.NewTokenizer(limitedReader)
	var title []byte
	// Is it time to give up yet?
	keepTrying := true
	for keepTrying {
		// Get next token
		tokenType := tokenizer.Next()
		if tokenType == html.StartTagToken {
			token := tokenizer.Token()
			switch token.Data {
			// We found title tag, get title data
			case "title":
				keepTrying = false
				tokenType = tokenizer.Next()
				if tokenType != html.TextToken {
					luaState.Push(lua.LNil)
					log.Printf("GET %s: wrong title token type: %s", u, tokenType)
					return 1
				}
				title = tokenizer.Text()
			// We reached body tag, stop processing
			case "body":
				keepTrying = false
			}
			// Parser error
		} else if tokenType == html.ErrorToken {
			luaState.Push(lua.LNil)
			log.Printf("GET %s: tokenizer error: %s", u, tokenizer.Err())
			return 1
		}
	}
	// We didn't meet error nor manage to find title
	if len(title) == 0 {
		luaState.Push(lua.LNil)
		log.Printf("GET %s: no title found", u)
		return 1
	}
	// Strip newlines and tabs from the title
	re := regexp.MustCompile(`[\n\t]`)
	title = re.ReplaceAll(title, []byte{})
	// Trim whitespace around the title
	strTitle := strings.TrimSpace(string(title))
	// Return up to 400 characters
	if len(strTitle) > 400 {
		strTitle = strTitle[:400]
	}
	luaState.Push(lua.LString(strTitle))
	return 1
}

// luaLibLoader returns a table containing our Lua library functions
func (b *BananaBoatBot) luaLibLoader(luaState *lua.LState) int {
	// Create map of function names to functions
	exports := map[string]lua.LGFunction{
		"get_title":    b.luaLibGetTitle,
		"luis_predict": b.luaLibLuisPredict,
		"owm":          b.luaLibOpenWeatherMap,
		"random":       b.luaLibRandom,
		"worker":       b.luaLibWorker,
	}
	// Convert map to Lua table and push to stack
	mod := luaState.SetFuncs(luaState.NewTable(), exports)
	luaState.Push(mod)
	return 1
}

type BananaBoatBotConfig struct {
	// Default port for IRC
	DefaultIrcPort int
	// Path to script to be loaded
	LuaFile string
	// Shall we log each received command or not
	LogCommands bool
	// Format String for Luis.ai URL
	LuisURLTemplate string
	// Maximum reconnect interval in seconds
	MaxReconnect int
	// Format String for OpenWeathermap URL
	OwmURLTemplate string
	// NewIrcServer creates a new irc server
	NewIrcServer func(parentCtx context.Context, serverName string, settings *client.IrcServerSettings) (client.IrcServerInterface, context.Context)
}

func (b *BananaBoatBot) newLuaState(ctx context.Context) *lua.LState {
	// Create new Lua state
	luaState := lua.NewState()
	luaState.SetContext(ctx)

	// Provide access to our library functions in Lua
	luaState.PreloadModule("bananaboat", b.luaLibLoader)

	return luaState
}

// NewBananaBoatBot creates a new BananaBoatBot
func NewBananaBoatBot(ctx context.Context, config *BananaBoatBotConfig) *BananaBoatBot {
	// Set default URLs of webservices
	if len(config.LuisURLTemplate) == 0 {
		config.LuisURLTemplate = "https://%s.api.cognitive.microsoft.com/luis/v2.0/apps/%s?subscription-key=%s&verbose=false&q=%s"
	}
	if len(config.OwmURLTemplate) == 0 {
		config.OwmURLTemplate = "https://api.openweathermap.org/data/2.5/weather?units=metric&APPID=%s&q=%s"
	}

	// We require a path to some script to load
	if len(config.LuaFile) == 0 {
		log.Fatal("Please specify script using -lua flag")
	}

	// Create BananaBoatBot
	b := BananaBoatBot{
		Config:   config,
		handlers: make(map[string]*lua.LFunction),
		nick:     "BananaBoatBot",
		realname: "Banana Boat Bot",
		username: "bananarama",
	}

	// Create new shared Lua state
	b.luaState = b.newLuaState(ctx)

	// Create new pool of Lua state
	b.luaPool = sync.Pool{
		New: func() interface{} {
			return b.newLuaState(ctx)
		},
	}

	// Create HTTP client
	b.httpClient = http.Client{
		Timeout: time.Second * 60,
	}

	// Call Lua script and process result
	err := b.ReloadLua(ctx)
	if err != nil {
		log.Printf("Lua error: %s", err)
	}

	// Return BananaBoatBot
	return &b
}
