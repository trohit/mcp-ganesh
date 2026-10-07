package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
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
		dbPath = filepath.Join(appDir, "data", "pgease.db")
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
			"description": "Git commit. paths: commit only these files. all=true: stage everything (git add -A). Neither: commit what is already staged.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"message": map[string]interface{}{
						"type":        "string",
						"description": "Commit message",
					},
					"paths": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Files to stage and commit (relative to repo)",
					},
					"all": map[string]interface{}{
						"type":        "boolean",
						"description": "Stage all changes incl. untracked (git add -A)",
					},
				},
				"required": []string{"message"},
			},
		},
		{
			"name":        "git_push",
			"description": "Git push to origin. Default branch: current (HEAD).",
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
			"name":        "git_diff",
			"description": "Git diff (output capped at 200KB)",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"paths":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Limit to these paths"},
					"staged": map[string]interface{}{"type": "boolean", "description": "Diff staged changes (--cached)"},
					"stat":   map[string]interface{}{"type": "boolean", "description": "Summary only (--stat)"},
					"ref":    map[string]interface{}{"type": "string", "description": "Compare against ref/commit (e.g. HEAD~1)"},
				},
			},
		},
		{
			"name":        "file_patch",
			"description": "Replace exact text in a file (str_replace). old_str must match exactly once unless replace_all.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":        map[string]interface{}{"type": "string", "description": "File path"},
					"old_str":     map[string]interface{}{"type": "string", "description": "Exact text to replace"},
					"new_str":     map[string]interface{}{"type": "string", "description": "Replacement text (may be empty)"},
					"replace_all": map[string]interface{}{"type": "boolean", "description": "Replace every occurrence"},
				},
				"required": []string{"path", "old_str", "new_str"},
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
			"description": "Execute command (no shell unless command=sh). Destructive patterns are blocked. Output returned even on failure.",
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
					"timeout_sec": map[string]interface{}{
						"type":        "integer",
						"description": "Kill after N seconds (default 25, max 600)",
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
	case "git_diff":
		result, errMsg = gitDiff(args)
	case "file_patch":
		result, errMsg = filePatch(args)
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
		text := errMsg
		if result != nil {
			if out := fmt.Sprintf("%v", result); out != "" {
				text += "\n" + out
			}
		}
		sendJSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"result": map[string]interface{}{
				"content": []map[string]interface{}{{"type": "text", "text": text}},
				"isError": true,
			},
		})
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
		if absPath == absAllowed || strings.HasPrefix(absPath, absAllowed+string(filepath.Separator)) {
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
	if !ok || msg == "" {
		return nil, "Missing message"
	}
	paths := strList(args["paths"])
	all, _ := args["all"].(bool)
	for _, p := range paths {
		if strings.HasPrefix(p, "-") {
			return nil, "Invalid path: " + p
		}
	}
	commitArgs := []string{"commit", "-m", msg}
	switch {
	case len(paths) > 0:
		if out, err := runIn(appDir, 60*time.Second, "git", append([]string{"add", "--"}, paths...)...); err != nil {
			return out, fmt.Sprintf("Git add error: %v", err)
		}
		commitArgs = append(append(commitArgs, "--"), paths...)
	case all:
		if out, err := runIn(appDir, 60*time.Second, "git", "add", "-A"); err != nil {
			return out, fmt.Sprintf("Git add error: %v", err)
		}
	}
	out, err := runIn(appDir, 120*time.Second, "git", commitArgs...)
	if err != nil {
		return out, fmt.Sprintf("Git commit error: %v", err)
	}
	return out, ""
}

func gitPush(args map[string]interface{}) (interface{}, string) {
	branch, _ := args["branch"].(string)
	if branch == "" {
		out, err := runIn(appDir, 10*time.Second, "git", "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return out, fmt.Sprintf("Cannot resolve current branch: %v", err)
		}
		branch = strings.TrimSpace(out)
	}
	if branch == "" || branch == "HEAD" || strings.HasPrefix(branch, "-") {
		return nil, "Detached HEAD or invalid branch; pass branch explicitly"
	}
	out, err := runIn(appDir, 120*time.Second, "git", "push", "origin", branch)
	out = "branch: " + branch + "\n" + out
	if err != nil {
		return out, fmt.Sprintf("Git push error: %v", err)
	}
	return out, ""
}

func gitDiff(args map[string]interface{}) (interface{}, string) {
	cmdArgs := []string{"diff", "--no-color"}
	if v, _ := args["staged"].(bool); v {
		cmdArgs = append(cmdArgs, "--cached")
	}
	if v, _ := args["stat"].(bool); v {
		cmdArgs = append(cmdArgs, "--stat")
	}
	if ref, _ := args["ref"].(string); ref != "" {
		if strings.HasPrefix(ref, "-") {
			return nil, "Invalid ref"
		}
		cmdArgs = append(cmdArgs, ref)
	}
	if paths := strList(args["paths"]); len(paths) > 0 {
		cmdArgs = append(append(cmdArgs, "--"), paths...)
	}
	out, err := runIn(appDir, 30*time.Second, "git", cmdArgs...)
	const maxOut = 200 * 1024
	if len(out) > maxOut {
		out = out[:maxOut] + "\n...[truncated]"
	}
	if err != nil {
		return out, fmt.Sprintf("Git diff error: %v", err)
	}
	if out == "" {
		out = "(no changes)"
	}
	return out, ""
}

func filePatch(args map[string]interface{}) (interface{}, string) {
	path, _ := args["path"].(string)
	oldStr, ok1 := args["old_str"].(string)
	newStr, ok2 := args["new_str"].(string)
	if path == "" || !ok1 || !ok2 || oldStr == "" {
		return nil, "Missing path, old_str or new_str"
	}
	if !isAllowedPath(path) {
		return nil, "Path not allowed"
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Sprintf("Stat error: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Sprintf("Read error: %v", err)
	}
	content := string(raw)
	n := strings.Count(content, oldStr)
	replaceAll, _ := args["replace_all"].(bool)
	if n == 0 {
		return nil, "old_str not found"
	}
	if n > 1 && !replaceAll {
		return nil, fmt.Sprintf("old_str matches %d times; widen it or set replace_all", n)
	}
	limit := 1
	if replaceAll {
		limit = -1
	}
	if err := os.WriteFile(path, []byte(strings.Replace(content, oldStr, newStr, limit)), info.Mode().Perm()); err != nil {
		return nil, fmt.Sprintf("Write error: %v", err)
	}
	if !replaceAll {
		n = 1
	}
	return fmt.Sprintf("Patched %d occurrence(s)", n), ""
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

	if _, err := os.Stat(dbPath); err != nil {
		return nil, "No database at " + dbPath + " (set DATABASE_PATH)"
	}
	// Read-only: mode=ro blocks writes even if a non-SELECT slips past the prefix check.
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
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

var denyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+(-\S+\s+)*-\S*[rR]\S*\s+(-\S+\s+)*(/|~|\*|/\*|\$HOME)(\s|$)`),
	regexp.MustCompile(`\bgit\s+push\b.*(\s--force(-with-lease)?\b|\s-f\b|\s\+\S)`),
	regexp.MustCompile(`\bgit\s+reset\s+.*--hard\b`),
	regexp.MustCompile(`\bgit\s+clean\s+-\S*f`),
	regexp.MustCompile(`\bmkfs`),
	regexp.MustCompile(`\bdd\s+.*\bof=/dev/`),
	regexp.MustCompile(`\b(shutdown|reboot|halt|poweroff)\b`),
	regexp.MustCompile(`:\(\)\s*\{`),
}

func execCommand(args map[string]interface{}) (interface{}, string) {
	command, ok := args["command"].(string)
	if !ok || command == "" {
		return nil, "Missing command"
	}
	dir := appDir
	if d, ok := args["dir"].(string); ok && d != "" {
		if !isAllowedPath(d) {
			return nil, "Dir not allowed (allowed: " + strings.Join(allowedDirs, ", ") + ")"
		}
		dir = d
	}
	cmdArgs := strList(args["args"])
	full := command + " " + strings.Join(cmdArgs, " ")
	for _, re := range denyPatterns {
		if re.MatchString(full) {
			return nil, "Blocked by denylist: " + re.String()
		}
	}
	timeout := 25 * time.Second
	if t, ok := args["timeout_sec"].(float64); ok && t > 0 {
		if t > 600 {
			t = 600
		}
		timeout = time.Duration(t) * time.Second
	}
	out, err := runIn(dir, timeout, command, cmdArgs...)
	if err != nil {
		return out, fmt.Sprintf("Exec error: %v", err)
	}
	return out, ""
}

func runIn(dir string, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("timed out after %s", timeout)
	}
	return string(out), err
}

func strList(v interface{}) []string {
	raw, _ := v.([]interface{})
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
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
