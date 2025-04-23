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

	// Add special print wrapper to maintain formatting
	wrappedCode := `
import builtins
import sys

# Create a custom print function that ensures space separation
original_print = builtins.print
def custom_print(*args, **kwargs):
    # Force flush to ensure output is immediately visible
    if 'flush' not in kwargs:
        kwargs['flush'] = True
    # Call the original print function
    original_print(*args, **kwargs)

builtins.print = custom_print

# Now execute the actual script
with open("/tmp/script.py") as f:
    script = f.read()
    exec(script)
`

	wrapperFile := fmt.Sprintf("/tmp/python-wrapper-%s.py", uuid.New().String())
	err = os.WriteFile(wrapperFile, []byte(wrappedCode), 0644)
	if err != nil {
		return fmt.Errorf("failed to write wrapper file: %v", err)
	}
	defer os.Remove(wrapperFile)

	// Copy to container
	wrapperCopyCmd := exec.Command("docker", "cp", wrapperFile, "online_compiler-python-runner-1:/tmp/wrapper.py")
	if err := wrapperCopyCmd.Run(); err != nil {
		return fmt.Errorf("failed to copy wrapper file to container: %v", err)
	}

	// Execute with the wrapper
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "online_compiler-python-runner-1", "python3", "-u", "/tmp/wrapper.py")
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
	// Use line-based reading for simpler output handling
	stdoutScanner := bufio.NewScanner(stdout)
	stderrScanner := bufio.NewScanner(stderr)

	// Channel to coordinate both readers
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	// Process stdout
	go func() {
		for stdoutScanner.Scan() {
			line := stdoutScanner.Text()

			// Send the line as a message
			c.sendMessage(line)

			// Check if this line might be an input prompt
			if strings.Contains(line, ":") || strings.Contains(line, "?") {
				c.sendMessage("WAITING_FOR_INPUT")
			}

		}

		if err := stdoutScanner.Err(); err != nil {
			log.Println("Stdout scanner error:", err)
		}

		close(stdoutDone)
	}()

	// Process stderr
	go func() {
		for stderrScanner.Scan() {
			c.sendMessage("Error: " + stderrScanner.Text())
		}

		if err := stderrScanner.Err(); err != nil {
			log.Println("Stderr scanner error:", err)
		}

		close(stderrDone)
	}()

	// Additionally monitor for partial lines that might be input prompts
	go func() {
		reader := bufio.NewReader(stdout)
		buffer := ""

		for {
			r, _, err := reader.ReadRune()
			if err != nil {
				if err != io.EOF {
					log.Println("Partial line monitor error:", err)
				}
				break
			}

			buffer += string(r)

			// If we see typical input prompt patterns without a newline
			if (strings.Contains(buffer, ": ") || strings.Contains(buffer, "?")) &&
				!strings.Contains(buffer, "\n") {
				c.sendMessage(buffer)
				c.sendMessage("WAITING_FOR_INPUT")
				buffer = ""
			} else if r == '\n' {
				buffer = ""
			}
		}
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
