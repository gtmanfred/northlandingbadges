// Package smtptest is an in-process SMTP server used to capture outbound mail in
// tests.
//
// It exists so the delivery path is exercised over a real socket with real SMTP
// verbs instead of a mock: the wire format is part of what can break. It binds to
// 127.0.0.1 only, which is also why no test can reach smtp.gmail.com (spec §6).
package smtptest

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
)

// Message is one captured SMTP transaction.
type Message struct {
	From string
	To   []string
	Data []byte
	// AuthUser is the username presented via AUTH PLAIN, if any.
	AuthUser string
	// AuthPass is the password presented via AUTH PLAIN, if any.
	AuthPass string
}

// Options configures the capture server.
type Options struct {
	// RequireAuth rejects MAIL FROM until AUTH PLAIN succeeds.
	RequireAuth bool
	// Username and Password, when set, are the only credentials accepted.
	Username string
	Password string
	// FailData makes DATA return a permanent error, to exercise send failures.
	FailData bool
}

// Server is a running capture server.
type Server struct {
	ln   net.Listener
	opts Options

	mu   sync.Mutex
	msgs []Message

	wg     sync.WaitGroup
	closed chan struct{}
}

// Start listens on a random localhost port and serves until Close.
func Start(opts Options) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("smtptest: listen: %w", err)
	}
	s := &Server{ln: ln, opts: opts, closed: make(chan struct{})}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Addr is the host:port to point a client at.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Messages returns a copy of everything captured so far.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.msgs))
	copy(out, s.msgs)
	return out
}

// Count is the number of captured messages.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// Close stops the server and waits for connections to drain.
func (s *Server) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
		close(s.closed)
	}
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
				return
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_ = s.handle(conn)
		}()
	}
}

type session struct {
	from     string
	to       []string
	authUser string
	authPass string
	authed   bool
}

func (s *Server) handle(conn net.Conn) error {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	reply := func(format string, args ...any) error {
		if _, err := fmt.Fprintf(w, format+"\r\n", args...); err != nil {
			return err
		}
		return w.Flush()
	}

	if err := reply("220 smtptest ready"); err != nil {
		return err
	}

	var sess session
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		verb, rest := split(line)

		switch strings.ToUpper(verb) {
		case "EHLO":
			if err := reply("250-smtptest greets %s", rest); err != nil {
				return err
			}
			if err := reply("250-SIZE 35882577"); err != nil {
				return err
			}
			if err := reply("250-8BITMIME"); err != nil {
				return err
			}
			if err := reply("250 AUTH PLAIN LOGIN"); err != nil {
				return err
			}
		case "HELO":
			if err := reply("250 smtptest"); err != nil {
				return err
			}
		case "AUTH":
			if err := s.handleAuth(&sess, rest, r, reply); err != nil {
				return err
			}
		case "MAIL":
			if s.opts.RequireAuth && !sess.authed {
				if err := reply("530 5.7.0 Authentication required"); err != nil {
					return err
				}
				continue
			}
			sess.from = extractAddress(rest)
			sess.to = nil
			if err := reply("250 2.1.0 Ok"); err != nil {
				return err
			}
		case "RCPT":
			sess.to = append(sess.to, extractAddress(rest))
			if err := reply("250 2.1.5 Ok"); err != nil {
				return err
			}
		case "DATA":
			if s.opts.FailData {
				if err := reply("554 5.0.0 Transaction failed"); err != nil {
					return err
				}
				continue
			}
			if err := reply("354 End data with <CR><LF>.<CR><LF>"); err != nil {
				return err
			}
			data, err := readData(r)
			if err != nil {
				return err
			}
			s.mu.Lock()
			s.msgs = append(s.msgs, Message{
				From: sess.from, To: sess.to, Data: data,
				AuthUser: sess.authUser, AuthPass: sess.authPass,
			})
			s.mu.Unlock()
			sess.from, sess.to = "", nil
			if err := reply("250 2.0.0 Ok: queued"); err != nil {
				return err
			}
		case "RSET":
			sess.from, sess.to = "", nil
			if err := reply("250 2.0.0 Ok"); err != nil {
				return err
			}
		case "NOOP":
			if err := reply("250 2.0.0 Ok"); err != nil {
				return err
			}
		case "QUIT":
			_ = reply("221 2.0.0 Bye")
			return nil
		default:
			if err := reply("500 5.5.2 Unrecognized command"); err != nil {
				return err
			}
		}
	}
}

func (s *Server) handleAuth(sess *session, rest string, r *bufio.Reader, reply func(string, ...any) error) error {
	mechanism, arg := split(rest)
	if !strings.EqualFold(mechanism, "PLAIN") {
		return reply("504 5.5.4 Unrecognized authentication type")
	}
	if arg == "" {
		if err := reply("334 "); err != nil {
			return err
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		arg = strings.TrimRight(line, "\r\n")
	}
	user, pass, err := decodePlain(arg)
	if err != nil {
		return reply("535 5.7.8 Authentication credentials invalid")
	}
	if s.opts.Username != "" && (user != s.opts.Username || pass != s.opts.Password) {
		return reply("535 5.7.8 Username and Password not accepted")
	}
	sess.authUser, sess.authPass, sess.authed = user, pass, true
	return reply("235 2.7.0 Accepted")
}

func decodePlain(encoded string) (user, pass string, err error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 {
		return "", "", errors.New("smtptest: malformed AUTH PLAIN payload")
	}
	return parts[1], parts[2], nil
}

// readData consumes the DATA payload up to the "." terminator, undoing dot
// stuffing as required by RFC 5321.
func readData(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			return out, nil
		}
		if strings.HasPrefix(trimmed, "..") {
			trimmed = trimmed[1:]
		}
		out = append(out, []byte(trimmed+"\r\n")...)
	}
}

func split(line string) (head, rest string) {
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

// extractAddress pulls the address out of "FROM:<a@b>" / "TO:<a@b>".
func extractAddress(arg string) string {
	if i := strings.Index(arg, "<"); i >= 0 {
		if j := strings.Index(arg[i:], ">"); j > 0 {
			return arg[i+1 : i+j]
		}
	}
	if i := strings.Index(arg, ":"); i >= 0 {
		return strings.TrimSpace(arg[i+1:])
	}
	return strings.TrimSpace(arg)
}
