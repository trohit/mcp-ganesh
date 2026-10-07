package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MCP Message types
type Message struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   interface{}     `json:"error,omitempty"`
}

type ToolCall struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type FileReadParams struct {
	Path string `json:"path"`
}

type FileWriteParams struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type GitParams struct {
	Command string `json:"command"`
	Message string `json:"message,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

type DockerParams struct {
	Command string `json:"command"`
	Service string `json:"service,omitempty"`
}

type DBQueryParams struct {
	Query string        `json:"query"`
	Args  []interface{} `json:"args,omitempty"`
}

type ExecParams struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Dir     string   `json:"dir,omitempty"`
}

var (
	appDir      = os.Getenv("APP_DIR")
	dbPath      = os.Getenv("DATABASE_PATH")
	allowedDirs []string
)

func init() {
	if appDir == "" {
		appDir = "/opt/app"
	}
	if dbPath == "" {
		dbPath = "/opt/app/data/pgease.db"
	}
	allowedDirs = []string{appDir}
}

func main() {
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			sendError(nil, -32700, fmt.Sprintf("Read error: %v", err))
			continue
		}

		var msg Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			sendError(nil, -32700, "Invalid JSON")
			continue
		}

		handleMessage(&msg)
	}
}

func handleMessage(msg *Message) {
	switch msg.Method {
	case "initialize":
		handleInitialize(msg)
	case "tools/list":
		handleToolsList(msg)
	case "tools/call":
		handleToolCall(msg)
	case "notifications/initialized":
		// no-op
	default:
		sendError(msg.ID, -32601, "Method not found")
	}
}

func handleInitialize(msg *Message) {
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      msg.ID,
		"result": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    "go-mcp-server",
				"version": "1.0.0",
			},
		},
	}
	sendJSON(response)
}

func handleToolsList(msg *Message) {
	tools := []map[string]interface{}{
		{
			"name":        "file_read",
			"description": "Read file contents (allowed paths: " + strings.Join(allowedDirs, ", ") + ")",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path to read",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "file_write",
			"description": "Write file contents",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path to write",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "File content",
					},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			"name":        "git_pull",
			"description": "Git pull from remote branch",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"branch": map[string]interface{}{
						"type":        "string",
						"description": "Branch to pull (default: current)",
					},
				},
			},
		},
		{
			"name":        "git_commit",
			"description": "Git commit changes",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"message": map[string]interface{}{
						"type":        "string",
						"description": "Commit message",
					},
				},
				"required": []string{"message"},
			},
		},
		{
			"name":        "git_push",
			"description": "Git push to remote",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"branch": map[string]interface{}{
						"type":        "string",
						"description": "Branch to push (default: current)",
					},
				},
			},
		},
		{
			"name":        "git_status",
			"description": "Get git status",
			"inputSchema": map[string]interface{}{
				"type": "object",
			},
		},
		{
			"name":        "docker_compose_up",
			"description": "Start docker-compose services",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"service": map[string]interface{}{
						"type":        "string",
						"description": "Service name (optional)",
					},
				},
			},
		},
		{
			"name":        "docker_compose_down",
			"description": "Stop docker-compose services",
			"inputSchema": map[string]interface{}{
				"type": "object",
			},
		},
		{
			"name":        "docker_compose_logs",
			"description": "Get docker-compose logs",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"lines": map[string]interface{}{
						"type":        "integer",
						"description": "Number of lines (default: 100)",
					},
					"service": map[string]interface{}{
						"type":        "string",
						"description": "Service name (optional)",
					},
				},
			},
		},
		{
			"name":        "docker_ps",
			"description": "List running containers",
			"inputSchema": map[string]interface{}{
				"type": "object",
			},
		},
		{
			"name":        "db_query",
			"description": "Execute database query (read-only)",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "SQL query (SELECT only)",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "exec",
			"description": "Execute shell command",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{
						"type":        "string",
						"description": "Command to execute",
					},
					"args": map[string]interface{}{
						"type":        "array",
						"description": "Command arguments",
					},
					"dir": map[string]interface{}{
						"type":        "string",
						"description": "Working directory",
					},
				},
				"required": []string{"command"},
			},
		},
	}

	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      msg.ID,
		"result": map[string]interface{}{
			"tools": tools,
		},
	}
	sendJSON(response)
}

func handleToolCall(msg *Message) {
	var params map[string]interface{}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		sendError(msg.ID, -32602, "Invalid params")
		return
	}

	toolName, ok := params["name"].(string)
	if !ok {
		sendError(msg.ID, -32602, "Missing tool name")
		return
	}

	var args map[string]interface{}
	if argsRaw, ok := params["arguments"].(map[string]interface{}); ok {
		args = argsRaw
	}

	var result interface{}
	var errMsg string

	switch toolName {
	case "file_read":
		result, errMsg = fileRead(args)
	case "file_write":
		result, errMsg = fileWrite(args)
	case "git_pull":
		result, errMsg = gitPull(args)
	case "git_commit":
		result, errMsg = gitCommit(args)
	case "git_push":
		result, errMsg = gitPush(args)
	case "git_status":
		result, errMsg = gitStatus()
	case "docker_compose_up":
		result, errMsg = dockerComposeUp(args)
	case "docker_compose_down":
		result, errMsg = dockerComposeDown()
	case "docker_compose_logs":
		result, errMsg = dockerComposeLogs(args)
	case "docker_ps":
		result, errMsg = dockerPS()
	case "db_query":
		result, errMsg = dbQuery(args)
	case "exec":
		result, errMsg = execCommand(args)
	default:
		errMsg = "Unknown tool"
	}

	if errMsg != "" {
		sendError(msg.ID, -32603, errMsg)
		return
	}

	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      msg.ID,
		"result": map[string]interface{}{
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": fmt.Sprintf("%v", result),
				},
			},
			"isError": false,
		},
	}
	sendJSON(response)
}

func isAllowedPath(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, allowed := range allowedDirs {
		absAllowed, _ := filepath.Abs(allowed)
		if strings.HasPrefix(absPath, absAllowed) {
			return true
		}
	}
	return false
}

func fileRead(args map[string]interface{}) (interface{}, string) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, "Missing path"
	}

	if !isAllowedPath(path) {
		return nil, "Path not allowed"
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Sprintf("Read error: %v", err)
	}

	return string(content), ""
}

func fileWrite(args map[string]interface{}) (interface{}, string) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, "Missing path"
	}

	if !isAllowedPath(path) {
		return nil, "Path not allowed"
	}

	content, ok := args["content"].(string)
	if !ok {
		return nil, "Missing content"
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return nil, fmt.Sprintf("Write error: %v", err)
	}

	return "File written", ""
}

func gitPull(args map[string]interface{}) (interface{}, string) {
	branch := "origin/HEAD"
	if b, ok := args["branch"].(string); ok {
		branch = b
	}

	cmd := exec.Command("git", "pull", branch)
	cmd.Dir = appDir
	output, err := cmd.CombinedOutput()

	if err != nil {
		return string(output), fmt.Sprintf("Git pull error: %v", err)
	}

	return string(output), ""
}

func gitCommit(args map[string]interface{}) (interface{}, string) {
	msg, ok := args["message"].(string)
	if !ok {
		return nil, "Missing message"
	}

	// Stage all changes
	stageCmd := exec.Command("git", "add", "-A")
	stageCmd.Dir = appDir
	if _, err := stageCmd.CombinedOutput(); err != nil {
		return nil, fmt.Sprintf("Git add error: %v", err)
	}

	// Commit
	commitCmd := exec.Command("git", "commit", "-m", msg)
	commitCmd.Dir = appDir
	output, err := commitCmd.CombinedOutput()

	if err != nil {
		return string(output), fmt.Sprintf("Git commit error: %v", err)
	}

	return string(output), ""
}

func gitPush(args map[string]interface{}) (interface{}, string) {
	branch := "main"
	if b, ok := args["branch"].(string); ok {
		branch = b
	}

	cmd := exec.Command("git", "push", "origin", branch)
	cmd.Dir = appDir
	output, err := cmd.CombinedOutput()

	if err != nil {
		return string(output), fmt.Sprintf("Git push error: %v", err)
	}

	return string(output), ""
}

func gitStatus() (interface{}, string) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = appDir
	output, err := cmd.CombinedOutput()

	if err != nil {
		return string(output), fmt.Sprintf("Git status error: %v", err)
	}

	return string(output), ""
}

func dockerComposeUp(args map[string]interface{}) (interface{}, string) {
	cmdArgs := []string{"compose", "up", "-d"}

	if service, ok := args["service"].(string); ok && service != "" {
		cmdArgs = append(cmdArgs, service)
	}

	cmd := exec.Command("docker", cmdArgs...)
	cmd.Dir = appDir
	output, err := cmd.CombinedOutput()

	if err != nil {
		return string(output), fmt.Sprintf("Docker compose up error: %v", err)
	}

	return string(output), ""
}

func dockerComposeDown() (interface{}, string) {
	cmd := exec.Command("docker", "compose", "down")
	cmd.Dir = appDir
	output, err := cmd.CombinedOutput()

	if err != nil {
		return string(output), fmt.Sprintf("Docker compose down error: %v", err)
	}

	return string(output), ""
}

func dockerComposeLogs(args map[string]interface{}) (interface{}, string) {
	lines := 100
	if l, ok := args["lines"].(float64); ok {
		lines = int(l)
	}

	cmdArgs := []string{"compose", "logs", "--tail", fmt.Sprintf("%d", lines)}

	if service, ok := args["service"].(string); ok && service != "" {
		cmdArgs = append(cmdArgs, service)
	}

	cmd := exec.Command("docker", cmdArgs...)
	cmd.Dir = appDir
	output, err := cmd.CombinedOutput()

	if err != nil {
		return string(output), fmt.Sprintf("Docker logs error: %v", err)
	}

	return string(output), ""
}

func dockerPS() (interface{}, string) {
	cmd := exec.Command("docker", "ps", "-a")
	output, err := cmd.CombinedOutput()

	if err != nil {
		return string(output), fmt.Sprintf("Docker ps error: %v", err)
	}

	return string(output), ""
}

func dbQuery(args map[string]interface{}) (interface{}, string) {
	query, ok := args["query"].(string)
	if !ok {
		return nil, "Missing query"
	}

	// Security: only allow SELECT
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "SELECT") {
		return nil, "Only SELECT queries allowed"
	}

	// Try SQLite first
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Sprintf("DB error: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Sprintf("Query error: %v", err)
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var results []map[string]interface{}

	for rows.Next() {
		values := make([]interface{}, len(cols))
		valuePtrs := make([]interface{}, len(cols))
		for i := range cols {
			valuePtrs[i] = &values[i]
		}

		rows.Scan(valuePtrs...)

		entry := make(map[string]interface{})
		for i, col := range cols {
			entry[col] = values[i]
		}
		results = append(results, entry)
	}

	return results, ""
}

func execCommand(args map[string]interface{}) (interface{}, string) {
	command, ok := args["command"].(string)
	if !ok {
		return nil, "Missing command"
	}

	dir := appDir
	if d, ok := args["dir"].(string); ok {
		dir = d
	}

	var cmdArgs []string
	if argsRaw, ok := args["args"].([]interface{}); ok {
		for _, arg := range argsRaw {
			if s, ok := arg.(string); ok {
				cmdArgs = append(cmdArgs, s)
			}
		}
	}

	cmd := exec.Command(command, cmdArgs...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()

	if err != nil {
		return string(output), fmt.Sprintf("Exec error: %v", err)
	}

	return string(output), ""
}

func sendJSON(v interface{}) {
	data, _ := json.Marshal(v)
	fmt.Fprintln(os.Stdout, string(data))
}

func sendError(id interface{}, code int, message string) {
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
	}
	sendJSON(response)
}
