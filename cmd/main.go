package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type Client struct {
	conn     *websocket.Conn
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	mu       sync.Mutex
	tempFile string
	done     chan struct{}
}

func main() {
	r := gin.Default()
	r.GET("/run", handleWebSocket)
	log.Println("Server starting on :7711...")
	if err := r.Run(":7711"); err != nil {
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

	client := &Client{
		conn: conn,
		done: make(chan struct{}),
	}
	defer client.cleanup()

	_, code, err := conn.ReadMessage()
	if err != nil {
		log.Println("Read code error:", err)
		return
	}

	if err := client.executePython(string(code)); err != nil {
		client.sendMessage("Error: " + err.Error())
	}
}

func (c *Client) executePython(code string) error {
	// Write temp file
	tempFile := fmt.Sprintf("/tmp/python-%s.py", uuid.New().String())
	c.tempFile = tempFile
	err := os.WriteFile(tempFile, []byte(code), 0644)
	if err != nil {
		return fmt.Errorf("failed to write temp file: %v", err)
	}

	// Copy to container
	copyCmd := exec.Command("docker", "cp", tempFile, "online_compiler-python-runner-1:/tmp/script.py")
	if err := copyCmd.Run(); err != nil {
		return fmt.Errorf("failed to copy file to container: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Using -u flag for unbuffered output to ensure we see prompts immediately
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "online_compiler-python-runner-1", "python3", "-u", "/tmp/script.py")
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

	c.stdin = stdin

	if err := cmd.Start(); err != nil {
		return err
	}

	// Handle output and input concurrently
	outputDone := make(chan struct{})
	go func() {
		c.readOutputs(stdout, stderr)
		close(outputDone)
	}()

	// Handle incoming input
	go c.handleUserInput()

	// Wait for program to finish
	err = cmd.Wait()
	close(c.done) // Signal input goroutine to stop

	// Wait for output processing to complete
	<-outputDone

	if err != nil && ctx.Err() != context.DeadlineExceeded {
		c.sendMessage("Execution finished with error: " + err.Error())
	} else {
		c.sendMessage("EXECUTION_COMPLETE")
	}

	return nil
}

// Combined function to read both stdout and stderr
func (c *Client) readOutputs(stdout, stderr io.Reader) {
	// Use a buffered reader that reads character by character
	stdoutReader := bufio.NewReader(stdout)
	stderrReader := bufio.NewReader(stderr)

	// Channel to coordinate both readers
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	go func() {
		buffer := ""
		for {
			r, _, err := stdoutReader.ReadRune()
			if err != nil {
				if err != io.EOF {
					log.Println("Stdout read error:", err)
				}
				break
			}

			buffer += string(r)

			// When we get a newline, send the buffer as a message
			if r == '\n' {
				c.sendMessage(strings.TrimRight(buffer, "\r\n"))
				buffer = ""
			} else if strings.Contains(buffer, ": ") || strings.Contains(buffer, "?") {
				// Possible input prompt detection
				c.sendMessage(buffer)
				c.sendMessage("WAITING_FOR_INPUT")
				buffer = ""
			}
		}

		// Send any remaining content
		if buffer != "" {
			c.sendMessage(buffer)
			// If the program ended with text and no newline, it's likely waiting for input
			c.sendMessage("WAITING_FOR_INPUT")
		}
		close(stdoutDone)
	}()

	go func() {
		scanner := bufio.NewScanner(stderrReader)
		for scanner.Scan() {
			c.sendMessage("Error: " + scanner.Text())
		}
		close(stderrDone)
	}()

	// Wait for both readers to finish
	<-stdoutDone
	<-stderrDone
}

func (c *Client) handleUserInput() {
	for {
		select {
		case <-c.done:
			return
		default:
			_, msg, err := c.conn.ReadMessage()
			if err != nil {
				log.Println("WebSocket read error:", err)
				c.killProcess()
				return
			}

			c.mu.Lock()
			if c.stdin != nil {
				_, err := fmt.Fprintf(c.stdin, "%s\n", msg)
				if err != nil {
					log.Println("Input write error:", err)
				}
			}
			c.mu.Unlock()
		}
	}
}

func (c *Client) killProcess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *Client) sendMessage(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.conn.WriteMessage(websocket.TextMessage, []byte(msg))
	if err != nil {
		log.Println("Write error:", err)
	}
}

func (c *Client) cleanup() {
	if c.tempFile != "" {
		_ = os.Remove(c.tempFile)
	}
	c.killProcess()
}
