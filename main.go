package main

import (
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Jeffail/gabs/v2"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/rivo/tview"
)

type Muxnet struct {
	sessionName           string
	responseDelay         time.Duration
	promptPattern         *regexp.Regexp
	processedPrompts      map[string]map[string]time.Time
	deduplicationInterval time.Duration
	sessionStatus         map[string]string
	app                   *tview.Application
	statusView            *tview.TextView
	daemonMode            bool
	logger                *log.Logger
	mu                    sync.Mutex
	watchedSessions       map[string]bool
	ophanim               *OphanimClient
}

type OphanimClient struct {
	SessionHash     string
	ModelConnection *websocket.Conn
	SessionHistory  *gabs.Container
	RAGMode         bool
	RAGQuery        string
	RAGSource       string
	SaveDir         string
}

func NewMuxnet(sessionName string, responseDelay time.Duration, daemonMode bool) *Muxnet {
	logger := log.New(os.Stdout, "MuxNet: ", log.Ldate|log.Ltime|log.Lshortfile)
	m := &Muxnet{
		sessionName:           sessionName,
		responseDelay:         responseDelay,
		promptPattern:         regexp.MustCompile(`.*#([$@%!])\s*(.+?)\s*\.`),
		processedPrompts:      make(map[string]map[string]time.Time),
		deduplicationInterval: 60 * time.Second,
		sessionStatus:         make(map[string]string),
		daemonMode:            daemonMode,
		logger:                logger,
		watchedSessions:       make(map[string]bool),
		ophanim:               NewOphanimClient(),
	}

	if !daemonMode {
		m.app = tview.NewApplication()
		m.statusView = tview.NewTextView().SetDynamicColors(true)
	}

	return m
}

func NewOphanimClient() *OphanimClient {
	return &OphanimClient{
		SessionHash:    uuid.New().String()[:11],
		SessionHistory: gabs.New(),
		RAGMode:        false,
		RAGQuery:       "Current Events",
		RAGSource:      "Google",
		SaveDir:        fmt.Sprintf("%s/.config/ophanim/", os.Getenv("HOME")),
	}
}

func (m *Muxnet) updateDisplay() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.daemonMode {
		if len(m.sessionStatus) > 0 {
			for sessionName, lastPrompt := range m.sessionStatus {
				m.logger.Printf("Session: %s, Last Prompt: %s\n", sessionName, lastPrompt)
			}
		}
	} else {
		m.statusView.Clear()
		fmt.Fprintf(m.statusView, "[yellow]MuxNet Status\n\n")
		if len(m.sessionStatus) > 0 {
			fmt.Fprintf(m.statusView, "[green]Active Sessions:\n")
			for sessionName, lastPrompt := range m.sessionStatus {
				fmt.Fprintf(m.statusView, "[white]%s: %s\n", sessionName, lastPrompt)
			}
		} else {
			fmt.Fprintf(m.statusView, "[yellow]No active tmux sessions found. Waiting for sessions...\n")
		}
		m.app.Draw()
	}
}

func (m *Muxnet) scanSessions() {
	for {
		sessions, err := m.listTmuxSessions()
		if err != nil {
			if m.daemonMode {
				m.logger.Printf("Error listing tmux sessions: %v", err)
			}
			m.sessionStatus = make(map[string]string)
		} else if len(sessions) == 0 {
			m.sessionStatus = make(map[string]string)
		} else {
			newStatus := make(map[string]string)
			for _, session := range sessions {
				m.monitorSession(session, newStatus)
			}
			m.sessionStatus = newStatus
		}

		m.updateDisplay()
		time.Sleep(m.responseDelay)
	}
}

func (m *Muxnet) listTmuxSessions() ([]string, error) {
	cmd := exec.Command("tmux", "list-sessions", "-F", "#{session_name}")
	output, err := cmd.Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			if exitError.ExitCode() == 1 {
				return []string{}, nil
			}
		}
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(output)), "\n"), nil
}

func (m *Muxnet) monitorSession(sessionName string, newStatus map[string]string) {
	m.setSessionLabel(sessionName, "👁️ ")
	m.watchedSessions[sessionName] = true

	content, err := m.capturePane(sessionName)
	if err != nil {
		m.logger.Printf("Error capturing pane for session %s: %v", sessionName, err)
		return
	}

	lastLine := m.getLastNonEmptyLine(content)
	if lastLine == "" {
		return
	}

	match := m.promptPattern.FindStringSubmatch(lastLine)
	if match == nil {
		return
	}

	glyph, prompt := match[1], match[2]
	currentTime := time.Now()

	if glyph == "!" {
		m.ophanim.DeleteSessionFile(m.sessionName)
		newStatus[sessionName] = "Session file deleted"
	} else if m.canExecutePrompt(sessionName, prompt, currentTime) {
		newStatus[sessionName] = prompt
		useRAG := glyph == "@"
		useScreenContent := glyph == "%"
		if useScreenContent {
			screenContent := m.getFilteredScreenContent(content)
			m.takeOver(sessionName, prompt, useRAG, screenContent)
		} else {
			m.takeOver(sessionName, prompt, useRAG, "")
		}
		if m.processedPrompts[sessionName] == nil {
			m.processedPrompts[sessionName] = make(map[string]time.Time)
		}
		m.processedPrompts[sessionName][prompt] = currentTime
	} else {
		newStatus[sessionName] = fmt.Sprintf("[Skipped] %s", prompt)
	}
}

func (m *Muxnet) setSessionLabel(sessionName, label string) {
	cmd := exec.Command("tmux", "set-option", "-t", sessionName, "status-left", label)
	err := cmd.Run()
	if err != nil {
		m.logger.Printf("Error setting label for session %s: %v", sessionName, err)
	}
}

func (m *Muxnet) capturePane(sessionName string) (string, error) {
	cmd := exec.Command("tmux", "capture-pane", "-p", "-t", sessionName)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func (m *Muxnet) getLastNonEmptyLine(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	var lastLine string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lastLine = line
		}
	}
	return lastLine
}

func (m *Muxnet) getFilteredScreenContent(content string) string {
	var filtered []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "#$") && !strings.Contains(line, "#@") && !strings.Contains(line, "#%") && !strings.Contains(line, "#!") {
			filtered = append(filtered, line)
		}
	}
	return strings.Join(filtered, "\\n")
}

func (m *Muxnet) canExecutePrompt(sessionName, prompt string, currentTime time.Time) bool {
	if m.processedPrompts[sessionName] == nil {
		return true
	}
	lastExecutionTime, exists := m.processedPrompts[sessionName][prompt]
	if !exists {
		return true
	}
	return currentTime.Sub(lastExecutionTime) > m.deduplicationInterval
}

func (m *Muxnet) takeOver(sessionName, prompt string, useRAG bool, screenContent string) {
	m.showProcessingMessage(sessionName)

	var fullPrompt string
	if screenContent != "" {
		fullPrompt = fmt.Sprintf("Screen Context:\\n%s\\n\\nUser Prompt: %s", screenContent, prompt)
	} else {
		fullPrompt = fmt.Sprintf("User Prompt: %s", prompt)
	}

	systemPrompt := "System Prompt: Provide the system commands necessary to achieve the user's goal stated below. Assume the user is on linux and provide ONLY the commands.\n NO explanations.\nNo sudo\n."
	fullPrompt = fmt.Sprintf("%s%s\n\n'''bash\n", systemPrompt, fullPrompt)

	m.ophanim.RAGMode = useRAG
	response := m.ophanim.PromptChatbot(fullPrompt, false)

	filteredResponse := m.filterCommandResponse(response)

	m.sendResponseToPane(sessionName, "\x03") // Send Ctrl+C
	m.sendResponseToPane(sessionName, filteredResponse)

	m.clearProcessingMessage(sessionName)
}

func (m *Muxnet) filterCommandResponse(response string) string {
	commandPattern := regexp.MustCompile(`^\s*[\w-]+`)
	var filteredLines []string
	scanner := bufio.NewScanner(strings.NewReader(response))
	for scanner.Scan() {
		line := scanner.Text()
		if commandPattern.MatchString(line) {
			filteredLines = append(filteredLines, line)
		}
	}
	return strings.Join(filteredLines, "\n")
}

func (m *Muxnet) showProcessingMessage(sessionName string) {
	exec.Command("tmux", "display-message", "-t", sessionName, "Processing...").Run()
}

func (m *Muxnet) clearProcessingMessage(sessionName string) {
	exec.Command("tmux", "display-message", "-t", sessionName, "").Run()
}

func (m *Muxnet) sendResponseToPane(sessionName, response string) {
	exec.Command("tmux", "send-keys", "-t", sessionName, response, "Enter").Run()
}

func (m *Muxnet) cleanup() {
	for sessionName := range m.watchedSessions {
		m.setSessionLabel(sessionName, "")
	}
}

func (o *OphanimClient) initWSClient() {
	server := LookupEnvOrString("OPHANIM_HOST", "ophanim.azai.run")
	port := LookupEnvOrString("OPHANIM_PORT", "443")
	proto := LookupEnvOrString("OPHANIM_PROTO", "wss")

	var err error
	o.ModelConnection, _, err = websocket.DefaultDialer.Dial(fmt.Sprintf("%s://%s:%s/queue/join", proto, server, port), nil)
	if err != nil {
		log.Fatal("Failed to connect to WebSocket server:", err)
	}

	_, message, err := o.ModelConnection.ReadMessage()
	if err != nil {
		return
	}
	if !strings.Contains(string(message), `"msg":"send_hash"`) {
		log.Fatal("Unexpected message from server:", string(message))
	}

	initialMsg := []byte(fmt.Sprintf(`{"fn_index": 4,"session_hash":"%s"}`, o.SessionHash))
	if err := o.ModelConnection.WriteMessage(websocket.TextMessage, initialMsg); err != nil {
		log.Fatal("Failed to send initial message:", err)
	}
}

func (o *OphanimClient) PromptChatbot(userInput string, hasChatbotSession bool) (modelResponse string) {
	o.initWSClient()
	defer o.ModelConnection.Close()

	for {
		_, message, err := o.ModelConnection.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				return modelResponse
			} else {
				log.Fatal("Failed to read message from server:", err)
				return modelResponse
			}
		}

		if strings.Contains(string(message), `"msg":"send_data"`) {
			nextMessage := o.constructClientMessage(userInput, hasChatbotSession)
			if nextMessage != "" {
				if err := o.ModelConnection.WriteMessage(websocket.TextMessage, []byte(nextMessage)); err != nil {
					log.Fatal("Failed to send message to server:", err)
				}
			}
		}

		if strings.Contains(string(message), `"msg":"process_starts"`) {
			continue
		}

		if strings.Contains(string(message), `"msg":"process_generating"`) {
			continue
		}

		if strings.Contains(string(message), `"msg":"process_completed"`) {
			parsedMessage, err := gabs.ParseJSON([]byte(message))
			if err != nil {
				log.Fatal("Failed to parse message from server:", err)
			}
			if parsedMessage.ExistsP("output.data") {
				lastEntry := len(parsedMessage.Path("output.data.0").Children()) - 1
				o.SessionHistory.ArrayAppendP(parsedMessage.Path(fmt.Sprintf("output.data.0.%d", lastEntry)).Children(), "output.data")
				modelResponse = parsedMessage.Path(fmt.Sprintf("output.data.0.%d", lastEntry)).Children()[1].Data().(string)
			} else {
				modelResponse = "No response from server"
				log.Println("No response from server")
			}
			return modelResponse
		}
	}
}
func (o *OphanimClient) constructClientMessage(userInput string, isContinuation bool) string {
	userInput = strings.ReplaceAll(userInput, "\n", "")
	userInput = strings.ReplaceAll(userInput, "\r", "")
	userInput = strings.ReplaceAll(userInput, "\x00", "")
	userInput = strings.ReplaceAll(userInput, "\x1a", "")
	userInput = strings.ReplaceAll(userInput, "'", "")
	userInput = strings.ReplaceAll(userInput, `"`, "")

	var RAG string
	if o.RAGMode {
		RAG = "true"
	} else {
		RAG = "false"
	}

	if !isContinuation {
		return fmt.Sprintf(`{"data":["","%s","%s",null,[["%s",""]],%s],"event_data":null,"fn_index":6,"session_hash":"%s"}`, o.RAGQuery, o.RAGSource, strings.TrimSpace(userInput), RAG, o.SessionHash)
	} else {
		if o.SessionHistory.ExistsP("output.data") {
			lastMessage := o.SessionHistory.Path("output.data").String()
			lastMessage = lastMessage[1 : len(lastMessage)-1]
			return fmt.Sprintf(`{"data":["","%s","%s",null,[%s,["%s",""]],%s],"event_data":null,"fn_index":6,"session_hash":"%s"}`, o.RAGQuery, o.RAGSource, lastMessage, strings.TrimSpace(userInput), RAG, o.SessionHash)
		}
		return ""
	}
}

func (o *OphanimClient) SaveChatHistory(fileName string) {
	chatHistoryFile := fmt.Sprintf("%s/%s.soul", o.SaveDir, fileName)
	if err := os.WriteFile(chatHistoryFile, []byte(o.SessionHistory.String()), 0644); err != nil {
		log.Printf("Failed to save chat history to file: %v", err)
		return
	}
}

func (o *OphanimClient) LoadChatHistory(fileName string) {
	chatHistoryFile := fmt.Sprintf("%s/%s.soul", o.SaveDir, fileName)
	chatHistory, err := os.ReadFile(chatHistoryFile)
	if err != nil {
		log.Printf("Failed to load chat history from file: %v", err)
		return
	}
	tempHistory, _ := gabs.ParseJSON(chatHistory)
	o.SessionHistory = tempHistory
}

func (o *OphanimClient) UndoLastInteraction() {
	if o.SessionHistory.ExistsP("output.data") {
		numberOfExchanges := len(o.SessionHistory.Path("output.data").Children())
		if numberOfExchanges > 1 {
			lastEntry := numberOfExchanges - 1
			o.SessionHistory.DeleteP(fmt.Sprintf("output.data.%d", lastEntry))
		}
	}
}

func (o *OphanimClient) ListChatHistory() {
	files, err := os.ReadDir(o.SaveDir)
	if err != nil {
		log.Printf("Failed to list chat history files: %v", err)
		return
	}
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".soul") {
			fmt.Println(strings.TrimSuffix(file.Name(), ".soul"))
		}
	}
}

func (o *OphanimClient) DeleteSessionFile(sessionName string) {
	filePath := fmt.Sprintf("%s/%s.soul", o.SaveDir, sessionName)
	err := os.Remove(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("Session file %s does not exist.", filePath)
		} else {
			log.Printf("Error deleting session file: %v", err)
		}
	} else {
		log.Printf("Session file %s deleted successfully.", filePath)
	}
}

func LookupEnvOrString(key string, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func generateSessionName() string {
	hash := md5.Sum([]byte(time.Now().String()))
	return hex.EncodeToString(hash[:])
}

func main() {
	sessionFlag := flag.String("session", "", "Specify a custom session name")
	delayFlag := flag.Float64("delay", 2, "Specify the response delay in seconds")
	daemonFlag := flag.Bool("d", false, "Run in daemon mode")
	flag.Parse()

	sessionName := *sessionFlag
	if sessionName == "" {
		sessionName = generateSessionName()
	}

	muxnet := NewMuxnet(sessionName, time.Duration(*delayFlag)*time.Second, *daemonFlag)

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		muxnet.logger.Printf("Received signal: %v", sig)
		if sig == syscall.SIGHUP && muxnet.daemonMode {
			muxnet.logger.Println("Ignoring SIGHUP in daemon mode")
		} else {
			muxnet.logger.Println("Shutting down...")
			muxnet.cleanup()
			if !muxnet.daemonMode {
				muxnet.app.Stop()
			}
			os.Exit(0)
		}
	}()

	if muxnet.daemonMode {
		muxnet.logger.Println("Starting MuxNet in daemon mode")
		muxnet.scanSessions()
	} else {
		muxnet.app.SetRoot(muxnet.statusView, true)
		go muxnet.scanSessions()

		if err := muxnet.app.Run(); err != nil {
			muxnet.logger.Fatalf("Error running application: %v", err)
		}
	}
}
