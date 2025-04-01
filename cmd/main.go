package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// WebSocket upgrader
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Client struct to manage WebSocket connection and process
type Client struct {
	conn     *websocket.Conn
	cmd      *exec.Cmd
	mu       sync.Mutex
	tempFile string
}

func main() {
	r := gin.Default()
	r.GET("/run", handleWebSocket)
	log.Println("Server starting on :8080...")
	if err := r.Run(":8080"); err != nil {
		log.Fatal("Server error:", err)
	}
}

func handleWebSocket(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	client := &Client{conn: conn}

	_, code, err := conn.ReadMessage()
	if err != nil {
		log.Println("Read error:", err)
		return
	}

	err = client.executePython(string(code))
	if err != nil {
		client.sendMessage("Error: " + err.Error())
	}
	if client.tempFile != "" {
		os.Remove(client.tempFile)
	}
}

func (c *Client) executePython(code string) error {
	// Generate a unique temporary file name
	tempFile := fmt.Sprintf("/tmp/python-%s.py", uuid.New().String())
	c.tempFile = tempFile

	// Write the code to the temporary file
	err := os.WriteFile(tempFile, []byte(code), 0644)
	if err != nil {
		return fmt.Errorf("failed to write temp file: %v", err)
	}

	// Copy the file to the python-runner container
	copyCmd := exec.Command("docker", "cp", tempFile, "online_compiler-python-runner-1:/tmp/script.py")
	if err := copyCmd.Run(); err != nil {
		return fmt.Errorf("failed to copy file to container: %v", err)
	}

	// Execute the file in the container
	cmd := exec.Command("docker", "exec", "-i", "online_compiler-python-runner-1", "python3", "/tmp/script.py")
	c.cmd = cmd

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			output := scanner.Text()
			c.sendMessage(output)
			if strings.HasSuffix(output, ": ") || strings.Contains(output, "Enter") {
				c.sendMessage("Waiting for input...")
			}
		}
	}()

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			c.sendMessage("Error: " + scanner.Text())
		}
	}()

	go func() {
		defer stdin.Close()
		for {
			_, msg, err := c.conn.ReadMessage()
			if err != nil {
				log.Println("WebSocket read error:", err)
				return
			}
			c.mu.Lock()
			_, err = fmt.Fprintf(stdin, "%s\n", string(msg))
			c.mu.Unlock()
			if err != nil {
				c.sendMessage("Input error: " + err.Error())
				return
			}
		}
	}()

	err = cmd.Wait()
	if err != nil {
		c.sendMessage("Execution finished with error: " + err.Error())
	}
	return nil
}

func (c *Client) sendMessage(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.conn.WriteMessage(websocket.TextMessage, []byte(msg))
	if err != nil {
		log.Println("Write error:", err)
	}
}
