package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"exe/internal/config"
	"exe/internal/keys"
	"exe/internal/vmm"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

type terminalVM struct {
	vmm.Manager
	addr string
}

func (v terminalVM) Get(context.Context, string) (*vmm.Info, error) {
	return &vmm.Info{Name: "guest", IP: "192.0.2.1", State: "running"}, nil
}

func (v terminalVM) DialGuest(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, v.addr)
}

// Exercise the actual WebSocket → guest SSH bridge: btop is an exec request,
// ordinary terminals remain shells, dimensions reach the guest, and a failed
// command is distinct from q so the browser can retain its diagnostic output.
func TestVMTerminalCommand(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		status        uint32
		disconnect    bool
	}{
		{name: "shell"},
		{name: "btop", command: "btop"},
		{name: "missing", command: "btop", status: 127},
		{name: "close window", command: "btop", disconnect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			keyPath, _, err := keys.Ensure(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key, err := os.ReadFile(keyPath)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.ParsePrivateKey(key)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &ssh.ServerConfig{NoClientAuth: true}
			cfg.AddHostKey(signer)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			requests := make(chan *ssh.Request, 16)
			ended := make(chan struct{})
			go func() {
				defer close(ended)
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				peer, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer peer.Close()
				go ssh.DiscardRequests(reqs)
				ch := <-chans
				if ch == nil {
					return
				}
				channel, reqs, err := ch.Accept()
				if err != nil {
					return
				}
				defer channel.Close()
				for req := range reqs {
					requests <- req
					req.Reply(true, nil)
					if req.Type == "exec" || req.Type == "shell" {
						go func() {
							if tc.status == 0 {
								io.WriteString(channel, "ready\r\n")
								var b [1]byte
								if _, err := io.ReadFull(channel, b[:]); err != nil {
									return
								}
							} else {
								io.WriteString(channel.Stderr(), "btop: command not found\r\n")
							}
							channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{tc.status}))
							channel.Close()
						}()
					}
				}
			}()
			s := New(&config.Config{SSHUser: "guest"}, terminalVM{addr: ln.Addr().String()}, nil, keyPath, t.TempDir())
			srv := httptest.NewServer(s.Handler())
			defer srv.Close()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/vms/guest/terminal?cmd="+tc.command, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			nextRequest := func(kind string) []byte {
				t.Helper()
				select {
				case r := <-requests:
					if r.Type != kind {
						t.Fatalf("SSH request = %q, want %q", r.Type, kind)
					}
					return r.Payload
				case <-ctx.Done():
					t.Fatalf("no SSH %s request", kind)
					return nil
				}
			}
			var pty struct {
				Term                      string
				Cols, Rows, Width, Height uint32
				Modes                     string
			}
			if err := ssh.Unmarshal(nextRequest("pty-req"), &pty); err != nil || pty.Cols != 80 || pty.Rows != 24 || pty.Term != "xterm-256color" {
				t.Fatalf("PTY = %+v, error %v", pty, err)
			}
			if tc.command == "" {
				nextRequest("shell")
			} else {
				var cmd struct{ Command string }
				if err := ssh.Unmarshal(nextRequest("exec"), &cmd); err != nil || cmd.Command != tc.command {
					t.Fatalf("command = %+v, error %v", cmd, err)
				}
			}
			if tc.disconnect {
				c.Close(websocket.StatusNormalClosure, "window closed")
				select {
				case <-ended:
				case <-ctx.Done():
					t.Fatal("guest SSH session survived closing its window")
				}
				return
			}
			if tc.status == 0 {
				if err := c.Write(ctx, websocket.MessageText, []byte(`{"resize":[100,30],"ping":42}`)); err != nil {
					t.Fatal(err)
				}
				var size struct{ Cols, Rows, Width, Height uint32 }
				if err := ssh.Unmarshal(nextRequest("window-change"), &size); err != nil || size.Cols != 100 || size.Rows != 30 {
					t.Fatalf("resize = %+v, error %v", size, err)
				}
			}
			var output strings.Builder
			for {
				typ, data, err := c.Read(ctx)
				if err != nil {
					var closeErr websocket.CloseError
					wantCode, wantReason := websocket.StatusNormalClosure, "session ended"
					if tc.status != 0 {
						wantCode, wantReason = websocket.StatusInternalError, "command failed"
					}
					if !errors.As(err, &closeErr) || closeErr.Code != wantCode || closeErr.Reason != wantReason {
						t.Fatalf("close = %v, want %d %q", err, wantCode, wantReason)
					}
					break
				}
				if typ == websocket.MessageBinary {
					output.Write(data)
				} else if string(data) == `{"pong":42}` {
					if err := c.Write(ctx, websocket.MessageBinary, []byte("q")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.status != 0 && !strings.Contains(output.String(), "command not found") {
				t.Fatalf("missing command diagnostic: %q", output.String())
			}
		})
	}
}
