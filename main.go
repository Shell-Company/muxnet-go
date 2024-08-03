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
	}

	if !daemonMode {
		m.app = tview.NewApplication()
		m.statusView = tview.NewTextView().SetDynamicColors(true)
	}

	return m
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
			// Only log the error in daemon mode
			if m.daemonMode {
				m.logger.Printf("Error listing tmux sessions: %v", err)
			}
			m.sessionStatus = make(map[string]string) // Clear existing status
		} else if len(sessions) == 0 {
			m.sessionStatus = make(map[string]string) // Clear existing status
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
			// tmux returns exit status 1 when there are no sessions
			if exitError.ExitCode() == 1 {
				return []string{}, nil
			}
		}
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(output)), "\n"), nil
}

func (m *Muxnet) monitorSession(sessionName string, newStatus map[string]string) {
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
		m.deleteSessionFile(sessionName)
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

	cmd := []string{"oph", "-prompt", fullPrompt, "-autoload", "-autosave", "-savefile", fmt.Sprintf("muxnet_%s", m.sessionName)}
	if useRAG {
		cmd = append(cmd, "-rag", "-rag_source", "Google", "-rag_query", prompt)
	}

	response, err := exec.Command(cmd[0], cmd[1:]...).Output()
	if err != nil {
		m.logger.Printf("Error executing Ophanim command: %v", err)
		m.sendResponseToPane(sessionName, fmt.Sprintf("Error executing Ophanim command: %v", err))
	} else {
		filteredResponse := m.filterCommandResponse(string(response))
		m.sendResponseToPane(sessionName, "\x03") // Send Ctrl+C
		m.sendResponseToPane(sessionName, filteredResponse)
	}

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

func (m *Muxnet) deleteSessionFile(sessionName string) {
	filePath := fmt.Sprintf("%s/.config/ophanim/muxnet_%s.soul", os.Getenv("HOME"), sessionName)
	err := os.Remove(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			m.sendResponseToPane(sessionName, fmt.Sprintf("Session file %s does not exist.", filePath))
		} else {
			m.sendResponseToPane(sessionName, fmt.Sprintf("Error deleting session file: %v", err))
		}
	} else {
		m.sendResponseToPane(sessionName, fmt.Sprintf("Session file %s deleted successfully.", filePath))
	}
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
