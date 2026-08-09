//go:build integration

package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

const (
	evalActorAddress          = "actor@eval.test"
	evalTargetAddress         = "target@eval.test"
	evalUnauthorizedAddress   = "unauthorized@outside.test"
	evalSubject               = "Question about fictional order ord_example_123"
	evalRequiredFact          = "Refunds are available within 30 days"
	maxSMTPCommandLine        = 4 << 10
	maxSMTPDataLine           = 64 << 10
	maxSMTPMessageBytes       = 2 << 20
	maxSMTPRecipients         = 10
	smtpReadBufferBytes       = 1024
	defaultSMTPForwardTimeout = 2 * time.Second
)

var (
	errSMTPLineTooLarge   = errors.New("SMTP line too large")
	errSMTPIncompleteLine = errors.New("incomplete SMTP line")
)

type forwardedSMTPMessage struct {
	From              string
	Recipients        []string
	Data              []byte
	ProviderMessageID string
}

type smtpForwarder struct {
	t        *testing.T
	listener net.Listener
	host     string
	port     int

	mu             sync.Mutex
	destination    string
	messages       []forwardedSMTPMessage
	connections    map[net.Conn]struct{}
	sequence       int
	wg             sync.WaitGroup
	forwardTimeout time.Duration
	closing        bool
	closeOnce      sync.Once
	closeDone      chan struct{}
}

func newSMTPForwarder(t *testing.T) *smtpForwarder {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start local SMTP fixture")
	}
	address := listener.Addr().(*net.TCPAddr)
	forwarder := &smtpForwarder{
		t:              t,
		listener:       listener,
		host:           "127.0.0.1",
		port:           address.Port,
		connections:    make(map[net.Conn]struct{}),
		forwardTimeout: defaultSMTPForwardTimeout,
		closeDone:      make(chan struct{}),
	}
	forwarder.wg.Add(1)
	go forwarder.serve()
	t.Cleanup(func() { <-forwarder.shutdown() })
	return forwarder
}

func (f *smtpForwarder) Host() string { return f.host }
func (f *smtpForwarder) Port() int    { return f.port }

func (f *smtpForwarder) setForwardTimeout(timeout time.Duration) {
	f.t.Helper()
	if timeout <= 0 {
		f.t.Fatal("SMTP forward timeout must be positive")
	}
	f.mu.Lock()
	f.forwardTimeout = timeout
	f.mu.Unlock()
}

func (f *smtpForwarder) shutdown() <-chan struct{} {
	f.closeOnce.Do(func() {
		f.mu.Lock()
		f.closing = true
		_ = f.listener.Close()
		for connection := range f.connections {
			_ = connection.Close()
		}
		f.mu.Unlock()
		go func() {
			f.wg.Wait()
			close(f.closeDone)
		}()
	})
	return f.closeDone
}

func (f *smtpForwarder) SetDestination(address string) {
	f.t.Helper()
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		f.t.Fatal("SMTP destination is not an explicit address")
	}
	ip := net.ParseIP(host)
	port, portErr := strconv.Atoi(portText)
	if ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 {
		f.t.Fatal("SMTP destination must be an explicit loopback address")
	}
	f.mu.Lock()
	f.destination = net.JoinHostPort(ip.String(), strconv.Itoa(port))
	f.mu.Unlock()
}

func (f *smtpForwarder) serve() {
	defer f.wg.Done()
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			return
		}
		remote, ok := connection.RemoteAddr().(*net.TCPAddr)
		if !ok || remote.IP == nil || !remote.IP.IsLoopback() {
			_ = connection.Close()
			continue
		}
		f.mu.Lock()
		if f.closing {
			f.mu.Unlock()
			_ = connection.Close()
			continue
		}
		f.connections[connection] = struct{}{}
		f.mu.Unlock()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer func() {
				f.mu.Lock()
				delete(f.connections, connection)
				f.mu.Unlock()
				_ = connection.Close()
			}()
			f.handle(connection)
		}()
	}
}

func smtpReply(writer io.Writer, code int, message string) bool {
	_, err := fmt.Fprintf(writer, "%d %s\r\n", code, message)
	return err == nil
}

func readSMTPLine(reader *bufio.Reader, maximum int) (string, error) {
	if maximum < 1 {
		return "", errSMTPLineTooLarge
	}
	line := make([]byte, 0, maximum)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > maximum-len(line) {
			return "", errSMTPLineTooLarge
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			if len(line) == 0 || line[len(line)-1] != '\n' {
				return "", errSMTPIncompleteLine
			}
			line = line[:len(line)-1]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			return string(line), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return "", errSMTPIncompleteLine
		default:
			return "", err
		}
	}
}

type stoppableEndlessReader struct {
	stop <-chan struct{}
}

func (r stoppableEndlessReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.stop:
		return 0, io.EOF
	default:
	}
	for index := range buffer {
		buffer[index] = 'x'
	}
	return len(buffer), nil
}

func TestReadSMTPLineBounded(t *testing.T) {
	t.Run("exact boundary and fragmented reads", func(t *testing.T) {
		const maximum = 32
		input := strings.Repeat("x", maximum-2) + "\r\n"
		line, err := readSMTPLine(bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader(input)), 16), maximum)
		if err != nil || line != strings.Repeat("x", maximum-2) {
			t.Fatalf("exact fragmented line = %q, %v", line, err)
		}
	})

	t.Run("oversized endless line returns without discard", func(t *testing.T) {
		stop := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := readSMTPLine(bufio.NewReaderSize(stoppableEndlessReader{stop: stop}, 16), 32)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, errSMTPLineTooLarge) {
				t.Fatalf("oversized line error = %v, want errSMTPLineTooLarge", err)
			}
		case <-time.After(100 * time.Millisecond):
			close(stop)
			<-done
			t.Fatal("oversized endless line was read without a bound")
		}
	})

	t.Run("EOF without newline is incomplete", func(t *testing.T) {
		_, err := readSMTPLine(bufio.NewReaderSize(strings.NewReader("incomplete"), 16), 32)
		if !errors.Is(err, errSMTPIncompleteLine) {
			t.Fatalf("incomplete line error = %v, want errSMTPIncompleteLine", err)
		}
	})
}

func TestReadSMTPDataRejectsOversizedHeaderAndBodyLines(t *testing.T) {
	for _, testCase := range []struct {
		name string
		data string
	}{
		{name: "header", data: "X-Synthetic: " + strings.Repeat("x", maxSMTPDataLine) + "\r\n\r\n.\r\n"},
		{name: "body", data: "Subject: Synthetic\r\n\r\n" + strings.Repeat("x", maxSMTPDataLine) + "\r\n.\r\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := readSMTPData(bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader(testCase.data)), 16))
			if !errors.Is(err, errSMTPLineTooLarge) {
				t.Fatalf("oversized DATA line error = %v, want errSMTPLineTooLarge", err)
			}
		})
	}

	exact := strings.Repeat("x", maxSMTPDataLine-2) + "\r\n.\r\n"
	data, err := readSMTPData(bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader(exact)), 16))
	if err != nil || len(data) != maxSMTPDataLine {
		t.Fatalf("exact DATA line bytes = %d, %v; want %d", len(data), err, maxSMTPDataLine)
	}
}

func dialSMTPFixture(t *testing.T, forwarder *smtpForwarder) (net.Conn, *bufio.Reader) {
	t.Helper()
	connection, err := net.DialTimeout("tcp4", net.JoinHostPort(forwarder.Host(), strconv.Itoa(forwarder.Port())), time.Second)
	if err != nil {
		t.Fatal("dial SMTP fixture")
	}
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		_ = connection.Close()
		t.Fatal("set SMTP fixture client deadline")
	}
	reader := bufio.NewReader(connection)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "220 ") {
		_ = connection.Close()
		t.Fatal("SMTP fixture omitted greeting")
	}
	return connection, reader
}

func writeSMTPCommand(t *testing.T, connection net.Conn, reader *bufio.Reader, command string, code string) {
	t.Helper()
	if _, err := io.WriteString(connection, command+"\r\n"); err != nil {
		t.Fatal("write SMTP fixture command")
	}
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, code+" ") {
		t.Fatalf("SMTP fixture response = %q, %v; want %s", line, err, code)
	}
}

func expectSMTPConnectionClosed(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err == nil {
		t.Fatalf("SMTP fixture kept rejected connection open with response %q", line)
	}
	if networkError, ok := err.(net.Error); ok && networkError.Timeout() {
		t.Fatal("SMTP fixture timed out instead of closing the rejected connection")
	}
}

func TestSMTPForwarderRejectsOversizedLinesAndRecovers(t *testing.T) {
	forwarder := newSMTPForwarder(t)

	t.Run("oversized command closes only its connection", func(t *testing.T) {
		connection, reader := dialSMTPFixture(t, forwarder)
		defer connection.Close()
		if _, err := io.WriteString(connection, strings.Repeat("X", maxSMTPCommandLine)+"\r\n"); err != nil {
			t.Fatal("write oversized SMTP command")
		}
		expectSMTPConnectionClosed(t, reader)
	})

	t.Run("oversized DATA line gets bounded rejection and close", func(t *testing.T) {
		connection, reader := dialSMTPFixture(t, forwarder)
		defer connection.Close()
		writeSMTPCommand(t, connection, reader, "EHLO sender.test", "250")
		writeSMTPCommand(t, connection, reader, "MAIL FROM:<sender@eval.test>", "250")
		writeSMTPCommand(t, connection, reader, "RCPT TO:<target@eval.test>", "250")
		writeSMTPCommand(t, connection, reader, "DATA", "354")
		if _, err := io.WriteString(connection, strings.Repeat("X", maxSMTPDataLine)+"\r\n"); err != nil {
			t.Fatal("write oversized SMTP DATA line")
		}
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "552 ") {
			t.Fatalf("oversized DATA response = %q, %v; want 552", line, err)
		}
		expectSMTPConnectionClosed(t, reader)
	})

	t.Run("listener recovers for a fresh connection", func(t *testing.T) {
		connection, reader := dialSMTPFixture(t, forwarder)
		defer connection.Close()
		writeSMTPCommand(t, connection, reader, "EHLO recovery.test", "250")
		writeSMTPCommand(t, connection, reader, "QUIT", "221")
	})
}

func TestSMTPForwarderStalledDestinationIsBounded(t *testing.T) {
	stalled, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("start stalled SMTP destination")
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseDestination := func() {
		releaseOnce.Do(func() {
			close(release)
			_ = stalled.Close()
		})
	}
	t.Cleanup(releaseDestination)
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := stalled.Accept()
		if acceptErr != nil {
			return
		}
		close(accepted)
		<-release
		_ = connection.Close()
	}()

	forwarder := newSMTPForwarder(t)
	forwarder.SetDestination(stalled.Addr().String())
	forwarder.setForwardTimeout(75 * time.Millisecond)
	connection, reader := dialSMTPFixture(t, forwarder)
	writeSMTPCommand(t, connection, reader, "EHLO sender.test", "250")
	writeSMTPCommand(t, connection, reader, "MAIL FROM:<sender@eval.test>", "250")
	writeSMTPCommand(t, connection, reader, "RCPT TO:<target@eval.test>", "250")
	writeSMTPCommand(t, connection, reader, "DATA", "354")

	started := time.Now()
	if _, err := io.WriteString(connection, "From: sender@eval.test\r\nTo: target@eval.test\r\nSubject: Synthetic stall\r\n\r\nbody\r\n.\r\n"); err != nil {
		t.Fatal("write stalled-destination message")
	}
	timer := time.AfterFunc(350*time.Millisecond, releaseDestination)
	line, readErr := reader.ReadString('\n')
	timer.Stop()
	elapsed := time.Since(started)
	if readErr != nil || !strings.HasPrefix(line, "451 ") {
		t.Fatalf("stalled-destination response = %q, %v; want 451", line, readErr)
	}
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("stalled destination returned after %s, want bounded before 250ms", elapsed)
	}
	select {
	case <-accepted:
	default:
		t.Fatal("SMTP fixture did not dial the stalled numeric destination")
	}
	_ = connection.Close()
	releaseDestination()
	select {
	case <-forwarder.shutdown():
	case <-time.After(time.Second):
		t.Fatal("SMTP fixture cleanup remained blocked after destination timeout")
	}
}

func smtpPath(line, prefix string, allowEmpty bool) (string, bool) {
	if len(line) < len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
		return "", false
	}
	rest := strings.TrimSpace(line[len(prefix):])
	if len(rest) < 2 || rest[0] != '<' || rest[len(rest)-1] != '>' {
		return "", false
	}
	address := rest[1 : len(rest)-1]
	if address == "" {
		return "", allowEmpty
	}
	if strings.ContainsAny(address, "<>\r\n \t") || strings.Count(address, "@") != 1 {
		return "", false
	}
	return address, true
}

func testRecipient(address string) bool {
	at := strings.LastIndexByte(address, '@')
	return at > 0 && strings.HasSuffix(strings.ToLower(address[at+1:]), ".test")
}

func (f *smtpForwarder) handle(connection net.Conn) {
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReaderSize(connection, smtpReadBufferBytes)
	if !smtpReply(connection, 220, "smtp.agents.localhost ready") {
		return
	}
	hello := false
	mailFrom := ""
	mailStarted := false
	var recipients []string

	for {
		line, err := readSMTPLine(reader, maxSMTPCommandLine)
		if err != nil {
			return
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO ") || strings.HasPrefix(upper, "HELO "):
			hello = true
			mailStarted = false
			mailFrom = ""
			recipients = nil
			if !smtpReply(connection, 250, "smtp.agents.localhost") {
				return
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			from, valid := smtpPath(line, "MAIL FROM:", true)
			if !hello || mailStarted || !valid {
				if !smtpReply(connection, 503, "bad sequence") {
					return
				}
				continue
			}
			mailStarted = true
			mailFrom = from
			recipients = nil
			if !smtpReply(connection, 250, "sender accepted") {
				return
			}
		case strings.HasPrefix(upper, "RCPT TO:"):
			recipient, valid := smtpPath(line, "RCPT TO:", false)
			if !hello || !mailStarted || !valid {
				if !smtpReply(connection, 503, "bad sequence") {
					return
				}
				continue
			}
			if !testRecipient(recipient) {
				if !smtpReply(connection, 550, "test recipients only") {
					return
				}
				continue
			}
			if len(recipients) >= maxSMTPRecipients {
				if !smtpReply(connection, 452, "too many recipients") {
					return
				}
				continue
			}
			duplicate := false
			for _, existing := range recipients {
				duplicate = duplicate || strings.EqualFold(existing, recipient)
			}
			if duplicate {
				if !smtpReply(connection, 550, "duplicate recipient") {
					return
				}
				continue
			}
			recipients = append(recipients, recipient)
			if !smtpReply(connection, 250, "recipient accepted") {
				return
			}
		case upper == "DATA":
			if !hello || !mailStarted || len(recipients) == 0 {
				if !smtpReply(connection, 503, "bad sequence") {
					return
				}
				continue
			}
			if !smtpReply(connection, 354, "send data") {
				return
			}
			data, dataErr := readSMTPData(reader)
			if dataErr != nil {
				_ = smtpReply(connection, 552, "message too large")
				return
			}
			f.mu.Lock()
			f.sequence++
			providerMessageID := fmt.Sprintf("<eval-synthetic-%d@agents.localhost>", f.sequence)
			f.mu.Unlock()
			data, dataErr = stampSyntheticProviderMessageID(data, providerMessageID)
			if dataErr != nil {
				_ = smtpReply(connection, 554, "invalid message identity")
				return
			}
			message := forwardedSMTPMessage{
				From: mailFrom, Recipients: append([]string(nil), recipients...), Data: data, ProviderMessageID: providerMessageID,
			}
			f.mu.Lock()
			f.messages = append(f.messages, message)
			destination := f.destination
			forwardTimeout := f.forwardTimeout
			f.mu.Unlock()
			if destination == "" {
				_ = smtpReply(connection, 451, "destination unavailable")
				return
			}
			if err := forwardSMTP(destination, mailFrom, recipients, data, forwardTimeout); err != nil {
				_ = smtpReply(connection, 451, "local forwarding failed")
				return
			}
			mailStarted = false
			mailFrom = ""
			recipients = nil
			if !smtpReply(connection, 250, "Ok "+providerMessageID) {
				return
			}
		case upper == "RSET":
			if !hello {
				if !smtpReply(connection, 503, "bad sequence") {
					return
				}
				continue
			}
			mailStarted = false
			mailFrom = ""
			recipients = nil
			if !smtpReply(connection, 250, "reset") {
				return
			}
		case upper == "QUIT":
			_ = smtpReply(connection, 221, "bye")
			return
		default:
			if !smtpReply(connection, 502, "unsupported command") {
				return
			}
		}
	}
}

func forwardSMTP(destination, mailFrom string, recipients []string, data []byte, timeout time.Duration) error {
	host, _, err := net.SplitHostPort(destination)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || timeout <= 0 {
		return errors.New("invalid SMTP forwarding destination")
	}
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp4", destination)
	if err != nil {
		return err
	}
	if err := connection.SetDeadline(deadline); err != nil {
		_ = connection.Close()
		return err
	}
	client, err := smtp.NewClient(connection, host)
	if err != nil {
		_ = connection.Close()
		return err
	}
	defer client.Close()
	if err := client.Hello("smtp.agents.localhost"); err != nil {
		return err
	}
	if err := client.Mail(mailFrom); err != nil {
		return err
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func readSMTPData(reader *bufio.Reader) ([]byte, error) {
	var data bytes.Buffer
	for {
		line, err := readSMTPLine(reader, maxSMTPDataLine)
		if err != nil {
			return nil, err
		}
		if line == "." {
			return append([]byte(nil), data.Bytes()...), nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		if data.Len()+len(line)+2 > maxSMTPMessageBytes {
			return nil, errors.New("SMTP message too large")
		}
		data.WriteString(line)
		data.WriteString("\r\n")
	}
}

func stampSyntheticProviderMessageID(data []byte, providerMessageID string) ([]byte, error) {
	message, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid SMTP fixture message")
	}
	if len(message.Header["Message-Id"]) != 0 {
		return nil, errors.New("SMTP fixture refuses duplicate Message-ID")
	}
	if !strings.HasPrefix(providerMessageID, "<eval-synthetic-") || !strings.HasSuffix(providerMessageID, "@agents.localhost>") ||
		strings.ContainsAny(providerMessageID, "\r\n\x00") {
		return nil, errors.New("invalid synthetic provider Message-ID")
	}
	stamped := make([]byte, 0, len(data)+len(providerMessageID)+14)
	stamped = append(stamped, "Message-ID: "...)
	stamped = append(stamped, providerMessageID...)
	stamped = append(stamped, "\r\n"...)
	stamped = append(stamped, data...)
	return stamped, nil
}

func (f *smtpForwarder) countRecipient(address string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, message := range f.messages {
		for _, recipient := range message.Recipients {
			if strings.EqualFold(recipient, address) {
				count++
			}
		}
	}
	return count
}

func (f *smtpForwarder) assertProviderMessageIDs(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	messages := append([]forwardedSMTPMessage(nil), f.messages...)
	f.mu.Unlock()
	if len(messages) < 2 {
		t.Fatal("SMTP fixture did not capture both round-trip deliveries")
	}
	for _, forwarded := range messages {
		parsed, err := mail.ReadMessage(bytes.NewReader(forwarded.Data))
		if err != nil {
			t.Fatal("SMTP fixture captured invalid delivered MIME")
		}
		values := parsed.Header["Message-Id"]
		if len(values) != 1 || values[0] != forwarded.ProviderMessageID {
			t.Fatal("SMTP fixture provider response and delivered Message-ID diverged")
		}
	}
}

func (f *smtpForwarder) latestMessageFor(t *testing.T, recipient string) forwardedSMTPMessage {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := len(f.messages) - 1; index >= 0; index-- {
		for _, candidate := range f.messages[index].Recipients {
			if strings.EqualFold(candidate, recipient) {
				message := f.messages[index]
				message.Recipients = append([]string(nil), message.Recipients...)
				message.Data = append([]byte(nil), message.Data...)
				return message
			}
		}
	}
	t.Fatal("SMTP fixture did not capture the expected recipient")
	return forwardedSMTPMessage{}
}

func assertOutboundIdentity(t *testing.T, forwarded forwardedSMTPMessage, envelopeFrom, headerAddress, displayName, replyTo string) {
	t.Helper()
	if forwarded.From != envelopeFrom {
		t.Fatalf("SMTP envelope sender = %q, want %q", forwarded.From, envelopeFrom)
	}
	message, err := mail.ReadMessage(bytes.NewReader(forwarded.Data))
	if err != nil {
		t.Fatal("SMTP fixture captured invalid MIME")
	}
	from, err := mail.ParseAddress(message.Header.Get("From"))
	if err != nil || from.Address != headerAddress || from.Name != displayName {
		t.Fatalf("From header = %q, want %q <%s>", message.Header.Get("From"), displayName, headerAddress)
	}
	parsedReplyTo, err := mail.ParseAddress(message.Header.Get("Reply-To"))
	if err != nil || parsedReplyTo.Address != replyTo {
		t.Fatalf("Reply-To header = %q, want %q", message.Header.Get("Reply-To"), replyTo)
	}
}

func dmarcPassAuthentication() *emailauth.Authentication {
	return passingAuthentication("eval.test")
}

func seedEvalAccount(t *testing.T, ts *testutil.E2ATestServer, actorAddress, targetAddress string) (*identity.APIKey, *identity.AgentIdentity, *identity.AgentIdentity) {
	t.Helper()
	ctx := context.Background()
	user, err := ts.Store.CreateOrGetUser(ctx, "owner@example.test", "Eval Owner", "synthetic-eval-owner")
	if err != nil {
		t.Fatal("seed synthetic eval user")
	}
	if _, err := ts.Store.ClaimOrCreateDomain(ctx, "eval.test", user.ID); err != nil {
		t.Fatal("seed synthetic eval domain")
	}
	if err := ts.Store.VerifyDomain(ctx, "eval.test", user.ID); err != nil {
		t.Fatal("verify synthetic eval domain")
	}
	if err := ts.Store.SetSendingStatus(ctx, "eval.test", "verified", "verified", "verified", "", nil); err != nil {
		t.Fatal("verify synthetic eval sending identity")
	}
	actor, err := ts.Store.CreateAgent(ctx, actorAddress, "eval.test", "Eval Actor", "", "local", user.ID)
	if err != nil {
		t.Fatal("seed synthetic actor")
	}
	target, err := ts.Store.CreateAgent(ctx, targetAddress, "eval.test", "Eval Target", "", "local", user.ID)
	if err != nil {
		t.Fatal("seed synthetic target")
	}
	key, err := ts.Store.CreateAPIKey(ctx, user.ID, "email-eval-round-trip", nil)
	if err != nil {
		t.Fatal("seed synthetic account key")
	}
	return key, actor, target
}

func setExactOutboundGate(t *testing.T, ts *testutil.E2ATestServer, agent *identity.AgentIdentity, allowlist []string) {
	t.Helper()
	_, err := ts.Store.UpdateAgentProtection(context.Background(), agent.ID, agent.UserID, identity.ProtectionConfig{
		InboundGatePolicy:       "open",
		InboundGateAction:       "flag",
		InboundScanSensitivity:  identity.SensitivityOff,
		OutboundGatePolicy:      "allowlist",
		OutboundAllowlist:       append([]string(nil), allowlist...),
		OutboundGateAction:      "block",
		OutboundScanSensitivity: identity.SensitivityOff,
		HITLTTLSeconds:          identity.HITLDefaultTTLSeconds,
		HITLExpirationAction:    identity.HITLExpirationReject,
	})
	if err != nil {
		t.Fatal("set exact synthetic outbound gate")
	}
}

func writeEvalSuite(t *testing.T, baseURL, actorAddress, targetAddress string) string {
	t.Helper()
	directory := t.TempDir()
	suite := fmt.Sprintf(`version: 1
name: local-email-round-trip
target: { email: %q }
actor: { email: %q }
transport:
  adapter: e2a
  api_key: ${E2A_EVAL_API_KEY}
  base_url: %q
  allowed_envelope_recipients: [%q, %q]
defaults:
  timeout: 20s
  settle: 300ms
  poll_interval: 1s
cases: [case.yaml]
`, targetAddress, actorAddress, baseURL, actorAddress, targetAddress)
	testCase := fmt.Sprintf(`id: deterministic-reply
send:
  subject: %q
  text: Can fictional order ord_example_123 be refunded?
expect:
  action: { kind: reply, count: 1 }
  sender:
    exactly: %q
    reply_to: { exactly: [%q] }
  recipients:
    to: { exactly: [%q] }
    cc: { exactly: [] }
    bcc: { exactly: [] }
    envelope: { exactly: [%q] }
  thread:
    in_reply_to: original
    references: contains_original
  subject: { policy: preserve }
  body:
    required_facts: [%q]
    plain_text: required
  attachments: { exactly: [] }
  timing: { reply_within: 15s }
  lifecycle:
    submission: sent
    actor_received: true
`, evalSubject, targetAddress, targetAddress, actorAddress, actorAddress, evalRequiredFact)
	if err := os.WriteFile(filepath.Join(directory, "suite.yaml"), []byte(suite), 0o600); err != nil {
		t.Fatal("write synthetic eval suite")
	}
	if err := os.WriteFile(filepath.Join(directory, "case.yaml"), []byte(testCase), 0o600); err != nil {
		t.Fatal("write synthetic eval case")
	}
	return filepath.Join(directory, "suite.yaml")
}

func emailEvalRuntimeDirectory(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve email eval runtime")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "plugins", "e2a", "skills", "email-evals", "runtime")
}

type boundedChildBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	remaining int
}

func newBoundedChildBuffer(limit int) *boundedChildBuffer {
	return &boundedChildBuffer{remaining: limit}
}

func (b *boundedChildBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(data)
	if len(data) > b.remaining {
		data = data[:b.remaining]
	}
	_, _ = b.buffer.Write(data)
	b.remaining -= len(data)
	return original, nil
}

func (b *boundedChildBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type deterministicResponder struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan error
	stdout *boundedChildBuffer
	once   sync.Once
}

func startDeterministicResponder(t *testing.T, baseURL, apiKey, targetAddress string) *deterministicResponder {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for the local eval responder")
	}
	ctx, cancel := context.WithCancel(context.Background())
	stdout := newBoundedChildBuffer(8 << 10)
	stderr := newBoundedChildBuffer(8 << 10)
	command := exec.CommandContext(ctx, node, filepath.Join(emailEvalRuntimeDirectory(t), "test", "live-responder.mjs"))
	command.Env = []string{
		"E2A_EVAL_API_KEY=" + apiKey,
		"E2A_EVAL_BASE_URL=" + baseURL,
		"E2A_EVAL_ACTOR=" + evalActorAddress,
		"E2A_EVAL_TARGET=" + targetAddress,
	}
	command.Stdout = stdout
	command.Stderr = stderr
	done := make(chan error, 1)
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal("start deterministic responder")
	}
	go func() { done <- command.Wait() }()
	responder := &deterministicResponder{t: t, cancel: cancel, done: done, stdout: stdout}
	t.Cleanup(func() { responder.stop() })
	return responder
}

func (r *deterministicResponder) stop() {
	r.once.Do(func() {
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
		}
	})
}

func (r *deterministicResponder) WaitForReply(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.done:
		r.once.Do(r.cancel)
		if err != nil || r.stdout.String() != "reply submitted\n" {
			t.Fatal("deterministic responder did not complete safely")
		}
	case <-time.After(25 * time.Second):
		r.stop()
		t.Fatal("deterministic responder timed out")
	}
}

type evalCLIResult struct {
	ExitCode     int
	Stderr       string
	RunDirectory string
}

func runEmailEvalCLI(t *testing.T, suite, apiKey string) evalCLIResult {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for the local eval runtime")
	}
	outputRoot := filepath.Join(filepath.Dir(suite), "results")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	stdout := newBoundedChildBuffer(64 << 10)
	stderr := newBoundedChildBuffer(16 << 10)
	command := exec.CommandContext(ctx, node,
		filepath.Join(emailEvalRuntimeDirectory(t), "cli.mjs"),
		"run", "--suite", suite, "--output", outputRoot, "--json")
	command.Env = []string{"E2A_EVAL_API_KEY=" + apiKey}
	command.Dir = filepath.Dir(suite)
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	result := evalCLIResult{Stderr: "email eval runtime failed safely"}
	if runErr == nil {
		result.ExitCode = 0
	} else if exitError, ok := runErr.(*exec.ExitError); ok {
		result.ExitCode = exitError.ExitCode()
	} else {
		result.ExitCode = 4
	}
	if ctx.Err() != nil {
		result.ExitCode = 4
		result.Stderr = "email eval runtime timed out safely"
	}
	switch stderr.String() {
	case "email-evals: assertion_failure\n":
		result.Stderr = "email eval runtime reported a fixed assertion-failure diagnostic"
	case "email-evals: transport_error\n":
		result.Stderr = "email eval runtime reported a fixed transport-error diagnostic"
	case "email-evals: target_timeout\n":
		result.Stderr = "email eval runtime reported a fixed target-timeout diagnostic"
	case "email-evals: grader_error\n":
		result.Stderr = "email eval runtime reported a fixed grader-error diagnostic"
	case "email-evals: unexpected runner failure\n":
		result.Stderr = "email eval runtime reported a fixed unexpected-runner diagnostic"
	}
	var output struct {
		Command string `json:"command"`
		Summary struct {
			Status string `json:"status"`
		} `json:"summary"`
	}
	parsedOutput := json.Unmarshal([]byte(stdout.String()), &output) == nil && output.Command == "run"
	if result.ExitCode == 0 && (!parsedOutput || output.Summary.Status != "pass") {
		result.ExitCode = 4
		result.Stderr = "email eval runtime returned an invalid result"
	}
	entries, readErr := os.ReadDir(outputRoot)
	if readErr == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "run_") {
				if result.RunDirectory != "" {
					result.ExitCode = 4
					result.Stderr = "email eval runtime produced ambiguous output"
					break
				}
				result.RunDirectory = filepath.Join(outputRoot, entry.Name())
			}
		}
	}
	if result.ExitCode == 0 && result.RunDirectory == "" {
		result.ExitCode = 4
		result.Stderr = "email eval runtime omitted its report"
	}
	return result
}

func assertReport(t *testing.T, runDirectory, wantStatus string, assertionIDs ...string) {
	t.Helper()
	entries, err := os.ReadDir(runDirectory)
	if err != nil {
		t.Fatal("read email eval report directory")
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	wantNames := []string{"cases.jsonl", "report.md", "summary.json"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("email eval artifact set = %v, want %v", names, wantNames)
	}
	summaryData, err := os.ReadFile(filepath.Join(runDirectory, "summary.json"))
	if err != nil {
		t.Fatal("read email eval summary")
	}
	var summary struct {
		Status   string `json:"status"`
		Complete bool   `json:"complete"`
	}
	if json.Unmarshal(summaryData, &summary) != nil || summary.Status != wantStatus || !summary.Complete {
		t.Fatal("email eval summary is not a complete pass")
	}
	casesData, err := os.ReadFile(filepath.Join(runDirectory, "cases.jsonl"))
	if err != nil {
		t.Fatal("read email eval case artifact")
	}
	var record struct {
		Status     string `json:"status"`
		Assertions []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"assertions"`
	}
	if json.Unmarshal(bytes.TrimSpace(casesData), &record) != nil || record.Status != wantStatus {
		t.Fatal("email eval case artifact is not a pass")
	}
	statuses := make(map[string]string, len(record.Assertions))
	for _, assertion := range record.Assertions {
		statuses[assertion.ID] = assertion.Status
	}
	for _, id := range assertionIDs {
		if statuses[id] != "pass" {
			t.Fatalf("email eval assertion %s = %q, want pass", id, statuses[id])
		}
	}
	if report, err := os.ReadFile(filepath.Join(runDirectory, "report.md")); err != nil || !bytes.Contains(report, []byte("- Status: **pass**")) {
		t.Fatal("email eval Markdown report is not a pass")
	}
}

func authedJSONWithIdempotency(t *testing.T, method, requestURL, apiKey, idempotencyKey, body string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, requestURL, strings.NewReader(body))
	if err != nil {
		t.Fatal("create synthetic idempotent request")
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal("perform synthetic idempotent request")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal("read synthetic idempotent response")
	}
	return response.StatusCode, responseBody
}

type durableEvalState struct {
	OutboundMessages    int
	OutboundJobs        int
	IdempotencyRows     int
	ReplyMessageID      string
	ReplySendJobID      int64
	ReplyIdemStatus     string
	ReplyResponseStatus int
	ReplyResponseBody   string
}

func readDurableEvalState(ctx context.Context, pool *pgxpool.Pool, userID, actorID, targetID string) (durableEvalState, error) {
	var state durableEvalState
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE agent_id IN ($1, $2) AND direction = 'outbound'`,
		actorID, targetID).Scan(&state.OutboundMessages); err != nil {
		return durableEvalState{}, err
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM river_job AS job
		   JOIN messages AS message ON message.id = job.args->>'message_id'
		  WHERE job.kind = 'outbound_send'
		    AND job.queue = 'outbound'
		    AND message.agent_id IN ($1, $2)`, actorID, targetID).Scan(&state.OutboundJobs); err != nil {
		return durableEvalState{}, err
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE user_id = $1`, userID).Scan(&state.IdempotencyRows); err != nil {
		return durableEvalState{}, err
	}
	if err := pool.QueryRow(ctx,
		`SELECT reply.id,
		        COALESCE(reply.send_job_id, 0),
		        idem.status,
		        idem.response_status,
		        convert_from(idem.response_body, 'UTF8')
		   FROM messages AS stimulus
		   JOIN idempotency_keys AS idem
		     ON idem.user_id = $1
		    AND idem.key = 'u:email-eval-reply-' || stimulus.id
		   JOIN messages AS reply
		     ON reply.id = (convert_from(idem.response_body, 'UTF8')::jsonb->>'message_id')
		  WHERE stimulus.agent_id = $2
		    AND stimulus.direction = 'inbound'
		    AND stimulus.subject = $3
		    AND reply.agent_id = $2
		    AND reply.direction = 'outbound'`,
		userID, targetID, evalSubject).Scan(
		&state.ReplyMessageID, &state.ReplySendJobID, &state.ReplyIdemStatus,
		&state.ReplyResponseStatus, &state.ReplyResponseBody); err != nil {
		return durableEvalState{}, err
	}
	return state, nil
}

func liveEvalOutboundJobs(ctx context.Context, pool *pgxpool.Pool, actorID, targetID string) (int, error) {
	var count int
	err := pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM river_job AS job
		   JOIN messages AS message ON message.id = job.args->>'message_id'
		  WHERE job.kind = 'outbound_send'
		    AND job.queue = 'outbound'
		    AND job.state::text IN ('available', 'running', 'retryable', 'scheduled')
		    AND message.agent_id IN ($1, $2)`, actorID, targetID).Scan(&count)
	return count, err
}

func waitForDurableEvalState(t *testing.T, pool *pgxpool.Pool, userID, actorID, targetID string, want durableEvalState) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	consecutive := 0
	var last durableEvalState
	var live int
	var lastErr error
	for {
		last, lastErr = readDurableEvalState(ctx, pool, userID, actorID, targetID)
		if lastErr == nil {
			live, lastErr = liveEvalOutboundJobs(ctx, pool, actorID, targetID)
		}
		if lastErr == nil && live == 0 && last == want {
			consecutive++
			if consecutive == 5 {
				return
			}
		} else {
			consecutive = 0
		}
		select {
		case <-ctx.Done():
			t.Fatalf("durable eval state did not settle: live_jobs=%d query_error=%v got=%+v want=%+v", live, lastErr, last, want)
		case <-ticker.C:
		}
	}
}

func assertOriginalDurableBaseline(t *testing.T, state durableEvalState) {
	t.Helper()
	if state.OutboundMessages != 2 || state.OutboundJobs != 2 {
		t.Fatalf("initial durable outbound state = %d messages/%d jobs, want 2/2", state.OutboundMessages, state.OutboundJobs)
	}
	var replay struct {
		Status    string `json:"status"`
		MessageID string `json:"message_id"`
	}
	if json.Unmarshal([]byte(state.ReplyResponseBody), &replay) != nil ||
		state.ReplyMessageID == "" || state.ReplySendJobID <= 0 || state.ReplyIdemStatus != "completed" ||
		state.ReplyResponseStatus != http.StatusAccepted || replay.Status != "accepted" || replay.MessageID != state.ReplyMessageID {
		t.Fatalf("initial durable reply outcome is incomplete: %+v", state)
	}
}

func assertUnauthorizedDurableAbsence(t *testing.T, pool *pgxpool.Pool, userID, targetID, idempotencyKey string) {
	t.Helper()
	var messages, idempotencyRows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM messages
		  WHERE agent_id = $1 AND direction = 'outbound' AND subject = 'Synthetic unauthorized attempt'`,
		targetID).Scan(&messages); err != nil {
		t.Fatal("count unauthorized durable messages")
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM idempotency_keys WHERE user_id = $1 AND key = $2`,
		userID, "u:"+idempotencyKey).Scan(&idempotencyRows); err != nil {
		t.Fatal("count unauthorized idempotency rows")
	}
	if messages != 0 || idempotencyRows != 0 {
		t.Fatalf("unauthorized durable side effects: messages=%d idempotency_rows=%d", messages, idempotencyRows)
	}
}

func TestEmailEvalRunnerRoundTrip(t *testing.T) {
	pool := testutil.TestDB(t)
	forwarder := newSMTPForwarder(t)
	ts := testutil.TestServer(t, pool,
		testutil.WithOutboundSMTP(forwarder.Host(), forwarder.Port(), "agents.localhost"),
		testutil.WithInboundAuthentication(dmarcPassAuthentication()),
	)
	forwarder.SetDestination(ts.SMTPAddr)

	apiKey, actor, target := seedEvalAccount(t, ts, evalActorAddress, evalTargetAddress)
	setExactOutboundGate(t, ts, actor, []string{target.EmailAddress()})
	setExactOutboundGate(t, ts, target, []string{actor.EmailAddress()})

	var suite string
	var durableBaseline durableEvalState
	var actorEgressBaseline, unauthorizedEgressBaseline int
	t.Run("actor to target reply round trip", func(t *testing.T) {
		suite = writeEvalSuite(t, ts.HTTPServer.URL, actor.EmailAddress(), target.EmailAddress())
		responder := startDeterministicResponder(t, ts.HTTPServer.URL, apiKey.PlaintextKey, target.EmailAddress())
		result := runEmailEvalCLI(t, suite, apiKey.PlaintextKey)
		responder.WaitForReply(t)
		if result.ExitCode != 0 {
			t.Fatalf("email eval failed: %s", result.Stderr)
		}
		assertReport(t, result.RunDirectory, "pass", "recipients.bcc",
			"thread.in_reply_to", "subject.policy", "body.required_facts")
		forwarder.assertProviderMessageIDs(t)
		assertOutboundIdentity(t, forwarder.latestMessageFor(t, target.EmailAddress()),
			"bounces@bounce.eval.test", actor.EmailAddress(), "Eval Actor", actor.EmailAddress())
		if got := forwarder.countRecipient(actor.EmailAddress()); got != 1 {
			t.Fatalf("reply SMTP egress count = %d, want 1", got)
		}
		var err error
		durableBaseline, err = readDurableEvalState(context.Background(), pool, actor.UserID, actor.ID, target.ID)
		if err != nil {
			t.Fatal("read original durable email-eval baseline")
		}
		assertOriginalDurableBaseline(t, durableBaseline)
		waitForDurableEvalState(t, pool, actor.UserID, actor.ID, target.ID, durableBaseline)
		actorEgressBaseline = forwarder.countRecipient(actor.EmailAddress())
		unauthorizedEgressBaseline = forwarder.countRecipient(evalUnauthorizedAddress)
	})

	t.Run("blocked unauthorized target has zero SMTP egress", func(t *testing.T) {
		const blockedKey = "email-eval-unauthorized-blocked"
		status, body := authedJSONWithIdempotency(t, "POST", sendURL(ts.HTTPServer.URL, target.EmailAddress()), apiKey.PlaintextKey, blockedKey,
			fmt.Sprintf(`{"to":[%q],"subject":"Synthetic unauthorized attempt","text":"Synthetic only"}`, evalUnauthorizedAddress))
		if status != 403 || !bytes.Contains(body, []byte(`"code":"blocked_by_policy"`)) {
			t.Fatalf("unauthorized target attempt status = %d, want blocked_by_policy", status)
		}
		waitForDurableEvalState(t, pool, actor.UserID, actor.ID, target.ID, durableBaseline)
		assertUnauthorizedDurableAbsence(t, pool, actor.UserID, target.ID, blockedKey)
		if after := forwarder.countRecipient(evalUnauthorizedAddress); after != unauthorizedEgressBaseline {
			t.Fatalf("unauthorized SMTP egress count changed from %d to %d", unauthorizedEgressBaseline, after)
		}
	})

	t.Run("stable reply idempotency has no duplicate or unauthorized SMTP egress", func(t *testing.T) {
		responder := startDeterministicResponder(t, ts.HTTPServer.URL, apiKey.PlaintextKey, target.EmailAddress())
		responder.WaitForReply(t)
		waitForDurableEvalState(t, pool, actor.UserID, actor.ID, target.ID, durableBaseline)
		if after := forwarder.countRecipient(actor.EmailAddress()); after != actorEgressBaseline {
			t.Fatalf("idempotent replay changed reply SMTP egress from %d to %d", actorEgressBaseline, after)
		}
		if after := forwarder.countRecipient(evalUnauthorizedAddress); after != unauthorizedEgressBaseline {
			t.Fatalf("idempotent replay changed unauthorized SMTP egress from %d to %d", unauthorizedEgressBaseline, after)
		}
	})

	t.Run("unverified sending status retains relay identity fallback", func(t *testing.T) {
		if err := ts.Store.SetSendingStatus(context.Background(), "eval.test", "pending", "", "", "", nil); err != nil {
			t.Fatal("set synthetic sending identity pending")
		}
		before := forwarder.countRecipient(target.EmailAddress())
		status, body := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, actor.EmailAddress()), apiKey.PlaintextKey,
			fmt.Sprintf(`{"to":[%q],"subject":"Synthetic relay fallback","text":"Synthetic only"}`, target.EmailAddress()))
		if status != 200 || !bytes.Contains(body, []byte(`"status":"sent"`)) {
			t.Fatalf("relay fallback send status = %d, want sent", status)
		}
		if after := forwarder.countRecipient(target.EmailAddress()); after != before+1 {
			t.Fatalf("relay fallback SMTP egress count = %d, want %d", after, before+1)
		}
		assertOutboundIdentity(t, forwarder.latestMessageFor(t, target.EmailAddress()),
			"agent@agents.localhost", "agent@agents.localhost", "Eval Actor via e2a", actor.EmailAddress())
	})
}
