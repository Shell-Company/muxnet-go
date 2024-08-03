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
	"regexp"
	"strings"
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
}

func NewMuxnet(sessionName string, responseDelay time.Duration) *Muxnet {
	return &Muxnet{
		sessionName:           sessionName,
		responseDelay:         responseDelay,
		promptPattern:         regexp.MustCompile(`.*#([$@%!])\s*(.+?)\s*\.`),
		processedPrompts:      make(map[string]map[string]time.Time),
		deduplicationInterval: 60 * time.Second,
		sessionStatus:         make(map[string]string),
		app:                   tview.NewApplication(),
		statusView:            tview.NewTextView().SetDynamicColors(true),
	}
}

func (m *Muxnet) updateDisplay() {
	m.statusView.Clear()
	fmt.Fprintf(m.statusView, "[yellow]MuxNet Status\n\n")
	fmt.Fprintf(m.statusView, "[green]Active Sessions:\n")
	for sessionName, lastPrompt := range m.sessionStatus {
		fmt.Fprintf(m.statusView, "[white]%s: %s\n", sessionName, lastPrompt)
	}
	m.app.Draw()
}

func (m *Muxnet) scanSessions() {
	for {
		sessions, err := m.listTmuxSessions()
		if err != nil {
			log.Printf("Error listing tmux sessions: %v", err)
			m.sessionStatus["Error"] = "Failed to list tmux sessions"
		} else if len(sessions) == 0 {
			m.sessionStatus["No active sessions"] = ""
		} else {
			for _, session := range sessions {
				m.monitorSession(session)
			}
		}

		m.updateDisplay()
		time.Sleep(m.responseDelay)
	}
}

func (m *Muxnet) listTmuxSessions() ([]string, error) {
	cmd := exec.Command("tmux", "list-sessions", "-F", "#{session_name}")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(output)), "\n"), nil
}

func (m *Muxnet) monitorSession(sessionName string) {
	content, err := m.capturePane(sessionName)
	if err != nil {
		log.Printf("Error capturing pane for session %s: %v", sessionName, err)
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
		m.sessionStatus[sessionName] = "Session file deleted"
	} else if m.canExecutePrompt(sessionName, prompt, currentTime) {
		m.sessionStatus[sessionName] = prompt
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
		m.sessionStatus[sessionName] = fmt.Sprintf("[Skipped] %s", prompt)
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
		log.Printf("Error executing Ophanim command: %v", err)
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
	flag.Parse()

	sessionName := *sessionFlag
	if sessionName == "" {
		sessionName = generateSessionName()
	}

	muxnet := NewMuxnet(sessionName, time.Duration(*delayFlag)*time.Second)

	muxnet.app.SetRoot(muxnet.statusView, true)
	go muxnet.scanSessions()

	if err := muxnet.app.Run(); err != nil {
		log.Fatalf("Error running application: %v", err)
	}
}
