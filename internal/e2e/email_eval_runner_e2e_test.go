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
	maxResponderResultBytes   = 128
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

type responderResult struct {
	Status    string `json:"status"`
	MessageID string `json:"message_id"`
}

func validSyntheticMessageID(value string) bool {
	if len(value) != len("msg_")+32 || !strings.HasPrefix(value, "msg_") {
		return false
	}
	for _, character := range value[len("msg_"):] {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func safeResponderStatus(value string) bool {
	if len(value) < 1 || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && character != '_' {
			return false
		}
	}
	return true
}

func parseResponderResult(output string) (responderResult, error) {
	if len(output) == 0 || len(output) > maxResponderResultBytes || output[len(output)-1] != '\n' {
		return responderResult{}, errors.New("invalid responder result framing")
	}
	var result responderResult
	if err := json.Unmarshal([]byte(output[:len(output)-1]), &result); err != nil {
		return responderResult{}, errors.New("invalid responder result JSON")
	}
	canonical, err := json.Marshal(result)
	if err != nil || output != string(canonical)+"\n" {
		return responderResult{}, errors.New("noncanonical responder result")
	}
	if !safeResponderStatus(result.Status) || !validSyntheticMessageID(result.MessageID) {
		return responderResult{}, errors.New("unsafe responder result fields")
	}
	return result, nil
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

func (r *deterministicResponder) WaitForReply(t *testing.T) responderResult {
	t.Helper()
	select {
	case err := <-r.done:
		r.once.Do(r.cancel)
		if err != nil {
			t.Fatal("deterministic responder did not complete safely")
		}
		result, parseErr := parseResponderResult(r.stdout.String())
		if parseErr != nil {
			t.Fatal("deterministic responder returned an invalid result")
		}
		return result
	case <-time.After(25 * time.Second):
		r.stop()
		t.Fatal("deterministic responder timed out")
	}
	return responderResult{}
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
	IdempotencyRows     int
	StimulusMessageID   string
	StimulusSendJobID   int64
	ReplyMessageID      string
	ReplySendJobID      int64
	ReplyIdemStatus     string
	ReplyResponseStatus int
	ReplyResponseBody   string
}

type outboundJobRecord struct {
	ID          int64
	Queue       string
	State       string
	Args        string
	Attempt     int64
	MaxAttempts int64
}

type outboundJobSnapshot struct {
	Jobs map[int64]outboundJobRecord
}

type outboundJobBaseline struct {
	IDs      map[int64]struct{}
	EvalJobs map[int64]outboundJobRecord
}

func cloneOutboundJobSnapshot(snapshot outboundJobSnapshot) outboundJobSnapshot {
	cloned := outboundJobSnapshot{Jobs: make(map[int64]outboundJobRecord, len(snapshot.Jobs))}
	for id, job := range snapshot.Jobs {
		cloned.Jobs[id] = job
	}
	return cloned
}

func readOutboundJobSnapshot(ctx context.Context, pool *pgxpool.Pool) (outboundJobSnapshot, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, queue, state::text, args::text, attempt, max_attempts
		   FROM river_job
		  WHERE kind = 'outbound_send'
		  ORDER BY id`)
	if err != nil {
		return outboundJobSnapshot{}, err
	}
	defer rows.Close()
	snapshot := outboundJobSnapshot{Jobs: make(map[int64]outboundJobRecord)}
	for rows.Next() {
		var job outboundJobRecord
		if err := rows.Scan(&job.ID, &job.Queue, &job.State, &job.Args, &job.Attempt, &job.MaxAttempts); err != nil {
			return outboundJobSnapshot{}, err
		}
		if job.ID <= 0 {
			return outboundJobSnapshot{}, errors.New("invalid outbound job identity")
		}
		if _, exists := snapshot.Jobs[job.ID]; exists {
			return outboundJobSnapshot{}, errors.New("duplicate outbound job identity")
		}
		snapshot.Jobs[job.ID] = job
	}
	if err := rows.Err(); err != nil {
		return outboundJobSnapshot{}, err
	}
	return snapshot, nil
}

func terminalRiverJobState(state string) bool {
	switch state {
	case "cancelled", "completed", "discarded":
		return true
	default:
		return false
	}
}

func liveOutboundJobs(snapshot outboundJobSnapshot) int {
	live := 0
	for _, job := range snapshot.Jobs {
		if !terminalRiverJobState(job.State) {
			live++
		}
	}
	return live
}

func waitForOutboundJobsTerminal(
	ctx context.Context,
	pollInterval time.Duration,
	read func(context.Context) (outboundJobSnapshot, error),
) (outboundJobSnapshot, error) {
	if pollInterval <= 0 {
		return outboundJobSnapshot{}, errors.New("invalid outbound job poll interval")
	}
	var lastErr error
	for {
		snapshot, err := read(ctx)
		if err == nil && liveOutboundJobs(snapshot) == 0 {
			return snapshot, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return outboundJobSnapshot{}, fmt.Errorf("outbound job terminal barrier: %w", lastErr)
			}
			return outboundJobSnapshot{}, errors.New("outbound jobs did not become terminal")
		case <-time.After(pollInterval):
		}
	}
}

func outboundJobMessageID(job outboundJobRecord) (string, error) {
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(job.Args), &args) != nil || len(args) != 1 {
		return "", errors.New("invalid outbound job args")
	}
	var messageID string
	if json.Unmarshal(args["message_id"], &messageID) != nil || messageID == "" {
		return "", errors.New("invalid outbound job message identity")
	}
	return messageID, nil
}

func validateExpectedEvalJob(job outboundJobRecord, jobID int64, messageID string) error {
	gotMessageID, err := outboundJobMessageID(job)
	if err != nil || job.ID != jobID || job.Queue != "outbound" || job.State != "completed" || gotMessageID != messageID {
		return errors.New("eval outbound job identity is incomplete")
	}
	return nil
}

func buildOutboundJobBaseline(pre, post outboundJobSnapshot, durable durableEvalState) (outboundJobBaseline, error) {
	additions := make(map[int64]outboundJobRecord)
	for id, job := range post.Jobs {
		if _, existed := pre.Jobs[id]; !existed {
			additions[id] = job
		}
	}
	if len(additions) != 2 || durable.StimulusSendJobID <= 0 || durable.ReplySendJobID <= 0 ||
		durable.StimulusSendJobID == durable.ReplySendJobID {
		return outboundJobBaseline{}, errors.New("email eval did not add exactly two distinct outbound jobs")
	}
	stimulus, stimulusExists := additions[durable.StimulusSendJobID]
	reply, replyExists := additions[durable.ReplySendJobID]
	if !stimulusExists || !replyExists {
		return outboundJobBaseline{}, errors.New("email eval outbound jobs do not match durable send job identities")
	}
	if err := validateExpectedEvalJob(stimulus, durable.StimulusSendJobID, durable.StimulusMessageID); err != nil {
		return outboundJobBaseline{}, err
	}
	if err := validateExpectedEvalJob(reply, durable.ReplySendJobID, durable.ReplyMessageID); err != nil {
		return outboundJobBaseline{}, err
	}
	baseline := outboundJobBaseline{
		IDs:      make(map[int64]struct{}, len(post.Jobs)),
		EvalJobs: map[int64]outboundJobRecord{stimulus.ID: stimulus, reply.ID: reply},
	}
	for id := range post.Jobs {
		baseline.IDs[id] = struct{}{}
	}
	return baseline, nil
}

func validateOutboundJobBaseline(baseline outboundJobBaseline, current outboundJobSnapshot) error {
	for id := range current.Jobs {
		if _, existed := baseline.IDs[id]; !existed {
			return errors.New("new outbound job appeared after the email eval baseline")
		}
	}
	for id, expected := range baseline.EvalJobs {
		if current.Jobs[id] != expected || !terminalRiverJobState(expected.State) {
			return errors.New("email eval outbound job changed or disappeared")
		}
	}
	return nil
}

func validateInitialResponderRelationship(initial responderResult, durable durableEvalState) error {
	if initial.Status != "sent" || initial.MessageID != durable.ReplyMessageID {
		return errors.New("initial responder result diverged from durable reply")
	}
	return nil
}

func validateResponderReplayRelationship(initial responderResult, durable durableEvalState, replay responderResult) error {
	if err := validateInitialResponderRelationship(initial, durable); err != nil {
		return err
	}
	var cached responderResult
	if json.Unmarshal([]byte(durable.ReplyResponseBody), &cached) != nil ||
		durable.ReplyResponseStatus != http.StatusAccepted || cached.Status != "accepted" ||
		cached.MessageID != durable.ReplyMessageID {
		return errors.New("durable reply cache is not the accepted outcome")
	}
	if replay.Status != "sent" || replay.Status != initial.Status || replay.MessageID != cached.MessageID {
		return fmt.Errorf("wait=sent replay diverged from the terminal SDK outcome: replay_status=%q initial_status=%q same_message_id=%t",
			replay.Status, initial.Status, replay.MessageID == cached.MessageID)
	}
	return nil
}

func TestOutboundJobOracleSettlesAcrossTransitionAndOldRowDeletion(t *testing.T) {
	const (
		stimulusMessageID = "msg_00000000000000000000000000000001"
		replyMessageID    = "msg_00000000000000000000000000000002"
	)
	job := func(id int64, state, messageID string) outboundJobRecord {
		return outboundJobRecord{
			ID: id, Queue: "outbound", State: state,
			Args: `{"message_id":"` + messageID + `"}`, Attempt: 1, MaxAttempts: 3,
		}
	}
	pre := outboundJobSnapshot{Jobs: map[int64]outboundJobRecord{
		10: job(10, "completed", "msg_00000000000000000000000000000010"),
		11: job(11, "completed", "msg_00000000000000000000000000000011"),
	}}
	mutatedOldJob := job(11, "completed", "msg_00000000000000000000000000000011")
	mutatedOldJob.Attempt = 2
	observations := []outboundJobSnapshot{
		{Jobs: map[int64]outboundJobRecord{
			10: job(10, "completed", "msg_00000000000000000000000000000010"),
			11: job(11, "completed", "msg_00000000000000000000000000000011"),
			21: job(21, "running", stimulusMessageID),
			22: job(22, "completed", replyMessageID),
		}},
		{Jobs: map[int64]outboundJobRecord{
			11: mutatedOldJob,
			21: job(21, "completed", stimulusMessageID),
			22: job(22, "completed", replyMessageID),
		}},
	}
	readIndex := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	post, err := waitForOutboundJobsTerminal(ctx, time.Nanosecond, func(context.Context) (outboundJobSnapshot, error) {
		if readIndex >= len(observations) {
			return observations[len(observations)-1], nil
		}
		snapshot := observations[readIndex]
		readIndex++
		return snapshot, nil
	})
	if err != nil {
		t.Fatalf("wait for terminal outbound jobs: %v", err)
	}
	if readIndex != 2 {
		t.Fatalf("terminal barrier used %d observations, want transition observation and terminal observation", readIndex)
	}
	durable := durableEvalState{
		StimulusMessageID: stimulusMessageID,
		StimulusSendJobID: 21,
		ReplyMessageID:    replyMessageID,
		ReplySendJobID:    22,
	}
	baseline, err := buildOutboundJobBaseline(pre, post, durable)
	if err != nil {
		t.Fatalf("build outbound job baseline after old-row deletion: %v", err)
	}
	if err := validateOutboundJobBaseline(baseline, post); err != nil {
		t.Fatalf("unchanged eval jobs failed validation: %v", err)
	}
	withOldJobDeleted := cloneOutboundJobSnapshot(post)
	delete(withOldJobDeleted.Jobs, 11)
	if err := validateOutboundJobBaseline(baseline, withOldJobDeleted); err != nil {
		t.Fatalf("unrelated old-job deletion failed validation: %v", err)
	}
	withUnexpectedCaptureJob := cloneOutboundJobSnapshot(post)
	withUnexpectedCaptureJob.Jobs[23] = job(23, "completed", "msg_00000000000000000000000000000023")
	if _, err := buildOutboundJobBaseline(pre, withUnexpectedCaptureJob, durable); err == nil {
		t.Fatal("unexpected third eval-round job passed baseline construction")
	}
	withWrongBinding := cloneOutboundJobSnapshot(post)
	withWrongBinding.Jobs[21] = job(21, "completed", replyMessageID)
	if _, err := buildOutboundJobBaseline(pre, withWrongBinding, durable); err == nil {
		t.Fatal("wrong stimulus job message binding passed baseline construction")
	}
	withNewJob := cloneOutboundJobSnapshot(withOldJobDeleted)
	withNewJob.Jobs[23] = job(23, "completed", "msg_00000000000000000000000000000023")
	if err := validateOutboundJobBaseline(baseline, withNewJob); err == nil {
		t.Fatal("new outbound job ID passed post-baseline validation")
	}
}

func TestOutboundJobOracleCapturesWrongQueueOrphansAndPendingFailClosed(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	const marker = "msg_fix2_oracle_"
	baselineSnapshot, err := readOutboundJobSnapshot(ctx, pool)
	if err != nil {
		t.Fatal("read baseline outbound job snapshot")
	}
	baselineLive := liveOutboundJobs(baselineSnapshot)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM river_job WHERE kind = 'outbound_send' AND args->>'message_id' LIKE $1`, marker+"%")
	})
	insert := func(suffix, queue, state string) int64 {
		t.Helper()
		var id int64
		var finalizedAt *time.Time
		if terminalRiverJobState(state) {
			now := time.Now()
			finalizedAt = &now
		}
		err := pool.QueryRow(ctx,
			`INSERT INTO river_job (args, kind, queue, state, max_attempts, finalized_at)
			 VALUES (jsonb_build_object('message_id', $1::text), 'outbound_send', $2::text, $3::river_job_state, 3, $4)
			 RETURNING id`, marker+suffix, queue, state, finalizedAt).Scan(&id)
		if err != nil {
			t.Fatalf("insert synthetic outbound oracle job: %v", err)
		}
		return id
	}
	wrongQueueID := insert("wrong_queue", "default", "completed")
	orphanID := insert("orphan", "outbound", "completed")
	pendingID := insert("pending", "outbound", "pending")

	snapshot, err := readOutboundJobSnapshot(ctx, pool)
	if err != nil {
		t.Fatal("read synthetic outbound job snapshot")
	}
	if len(snapshot.Jobs) != len(baselineSnapshot.Jobs)+3 {
		t.Fatalf("outbound job snapshot count = %d, want baseline %d + 3", len(snapshot.Jobs), len(baselineSnapshot.Jobs))
	}
	for _, want := range []struct {
		id, attempt, maxAttempts int64
		queue, state, messageID  string
	}{
		{id: wrongQueueID, queue: "default", state: "completed", messageID: marker + "wrong_queue", maxAttempts: 3},
		{id: orphanID, queue: "outbound", state: "completed", messageID: marker + "orphan", maxAttempts: 3},
		{id: pendingID, queue: "outbound", state: "pending", messageID: marker + "pending", maxAttempts: 3},
	} {
		got, exists := snapshot.Jobs[want.id]
		if !exists {
			t.Fatalf("outbound job snapshot omitted job %d", want.id)
		}
		messageID, messageErr := outboundJobMessageID(got)
		if messageErr != nil || got.Queue != want.queue || got.State != want.state ||
			got.Attempt != want.attempt || got.MaxAttempts != want.maxAttempts || messageID != want.messageID {
			t.Fatalf("outbound job snapshot fields diverged for job %d", want.id)
		}
	}
	live := liveOutboundJobs(snapshot)
	if live != baselineLive+1 {
		t.Fatalf("live outbound jobs = %d; want baseline %d + pending job", live, baselineLive)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1`, pendingID); err != nil {
		t.Fatal("finalize synthetic pending job")
	}
	snapshot, err = readOutboundJobSnapshot(ctx, pool)
	if err != nil {
		t.Fatal("read finalized outbound job snapshot")
	}
	if live = liveOutboundJobs(snapshot); live != baselineLive {
		t.Fatalf("live outbound jobs after finalize = %d; want baseline %d", live, baselineLive)
	}
	for _, state := range []string{"cancelled", "completed", "discarded"} {
		probe := outboundJobSnapshot{Jobs: map[int64]outboundJobRecord{1: {ID: 1, State: state}}}
		if liveOutboundJobs(probe) != 0 {
			t.Fatalf("terminal River state %q counted live", state)
		}
	}
	for _, state := range []string{"available", "pending", "retryable", "running", "scheduled", "future_state"} {
		probe := outboundJobSnapshot{Jobs: map[int64]outboundJobRecord{1: {ID: 1, State: state}}}
		if liveOutboundJobs(probe) != 1 {
			t.Fatalf("live or unknown River state %q was not counted live", state)
		}
	}
}

func TestResponderWaitSentReplayRelationship(t *testing.T) {
	const messageID = "msg_0123456789abcdef0123456789abcdef"
	initial, err := parseResponderResult(`{"status":"sent","message_id":"` + messageID + `"}` + "\n")
	if err != nil {
		t.Fatalf("parse canonical responder result: %v", err)
	}
	replay, err := parseResponderResult(`{"status":"sent","message_id":"` + messageID + `"}` + "\n")
	if err != nil {
		t.Fatalf("parse canonical replay result: %v", err)
	}
	durable := durableEvalState{
		ReplyMessageID:      messageID,
		ReplyResponseStatus: http.StatusAccepted,
		ReplyResponseBody:   `{"status":"accepted","message_id":"` + messageID + `"}`,
	}
	if err := validateResponderReplayRelationship(initial, durable, replay); err != nil {
		t.Fatalf("valid responder replay relationship: %v", err)
	}
	for _, testCase := range []struct {
		name    string
		initial responderResult
	}{
		{name: "initial message id", initial: responderResult{Status: "sent", MessageID: "msg_ffffffffffffffffffffffffffffffff"}},
		{name: "initial status", initial: responderResult{Status: "accepted", MessageID: messageID}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if validateResponderReplayRelationship(testCase.initial, durable, replay) == nil {
				t.Fatal("mismatched initial result passed validation")
			}
		})
	}
	for _, testCase := range []struct {
		name   string
		replay responderResult
	}{
		{name: "message id", replay: responderResult{Status: "sent", MessageID: "msg_ffffffffffffffffffffffffffffffff"}},
		{name: "status", replay: responderResult{Status: "accepted", MessageID: messageID}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if validateResponderReplayRelationship(initial, durable, testCase.replay) == nil {
				t.Fatal("mismatched replay result passed validation")
			}
		})
	}
}

func TestResponderResultValidationRejectsUnsafeShape(t *testing.T) {
	const messageID = "msg_0123456789abcdef0123456789abcdef"
	for _, unsafe := range []string{
		`{"status":"sent","message_id":"` + messageID + `","extra":true}` + "\n",
		`{"message_id":"` + messageID + `","status":"sent"}` + "\n",
		`{"status":"sent","message_id":"msg_bad"}` + "\n",
		strings.Repeat("x", maxResponderResultBytes+1),
	} {
		if _, err := parseResponderResult(unsafe); err == nil {
			t.Fatal("unsafe responder result passed validation")
		}
	}
}

func readDurableEvalState(ctx context.Context, pool *pgxpool.Pool, userID, actorID, targetID string) (durableEvalState, error) {
	var state durableEvalState
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE agent_id IN ($1, $2) AND direction = 'outbound'`,
		actorID, targetID).Scan(&state.OutboundMessages); err != nil {
		return durableEvalState{}, err
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE user_id = $1`, userID).Scan(&state.IdempotencyRows); err != nil {
		return durableEvalState{}, err
	}
	if err := pool.QueryRow(ctx,
		`SELECT id, COALESCE(send_job_id, 0)
		   FROM messages
		  WHERE agent_id = $1
		    AND direction = 'outbound'
		    AND subject = $2`, actorID, evalSubject).Scan(
		&state.StimulusMessageID, &state.StimulusSendJobID); err != nil {
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

func waitForDurableEvalState(
	t *testing.T,
	pool *pgxpool.Pool,
	userID, actorID, targetID string,
	want durableEvalState,
	jobsBaseline outboundJobBaseline,
) {
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
			var snapshot outboundJobSnapshot
			snapshot, lastErr = readOutboundJobSnapshot(ctx, pool)
			if lastErr == nil {
				if err := validateOutboundJobBaseline(jobsBaseline, snapshot); err != nil {
					t.Fatalf("outbound job safety baseline violated: %v", err)
				}
				live = liveOutboundJobs(snapshot)
			}
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
			replyEqual := last.ReplyMessageID == want.ReplyMessageID &&
				last.ReplySendJobID == want.ReplySendJobID && last.ReplyIdemStatus == want.ReplyIdemStatus &&
				last.ReplyResponseStatus == want.ReplyResponseStatus && last.ReplyResponseBody == want.ReplyResponseBody
			stimulusEqual := last.StimulusMessageID == want.StimulusMessageID &&
				last.StimulusSendJobID == want.StimulusSendJobID
			t.Fatalf("durable eval state did not settle: live_jobs=%d query_error=%v messages=%d/%d idempotency_rows=%d/%d stimulus_equal=%t reply_equal=%t",
				live, lastErr, last.OutboundMessages, want.OutboundMessages,
				last.IdempotencyRows, want.IdempotencyRows, stimulusEqual, replyEqual)
		case <-ticker.C:
		}
	}
}

func assertOriginalDurableBaseline(t *testing.T, state durableEvalState) {
	t.Helper()
	if state.OutboundMessages != 2 {
		t.Fatalf("initial durable outbound messages = %d, want 2", state.OutboundMessages)
	}
	var replay struct {
		Status    string `json:"status"`
		MessageID string `json:"message_id"`
	}
	if json.Unmarshal([]byte(state.ReplyResponseBody), &replay) != nil ||
		state.StimulusMessageID == "" || state.StimulusSendJobID <= 0 ||
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
	preEvalJobs, err := readOutboundJobSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatal("read pre-eval outbound job snapshot")
	}
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
	var jobsBaseline outboundJobBaseline
	var initialResponderResult responderResult
	var actorEgressBaseline, unauthorizedEgressBaseline int
	t.Run("actor to target reply round trip", func(t *testing.T) {
		suite = writeEvalSuite(t, ts.HTTPServer.URL, actor.EmailAddress(), target.EmailAddress())
		responder := startDeterministicResponder(t, ts.HTTPServer.URL, apiKey.PlaintextKey, target.EmailAddress())
		result := runEmailEvalCLI(t, suite, apiKey.PlaintextKey)
		initialResponderResult = responder.WaitForReply(t)
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
		barrierCtx, barrierCancel := context.WithTimeout(context.Background(), 5*time.Second)
		postEvalJobs, err := waitForOutboundJobsTerminal(barrierCtx, 50*time.Millisecond, func(ctx context.Context) (outboundJobSnapshot, error) {
			return readOutboundJobSnapshot(ctx, pool)
		})
		barrierCancel()
		if err != nil {
			t.Fatalf("wait for outbound job terminal barrier: %v", err)
		}
		durableBaseline, err = readDurableEvalState(context.Background(), pool, actor.UserID, actor.ID, target.ID)
		if err != nil {
			t.Fatal("read original durable email-eval baseline")
		}
		assertOriginalDurableBaseline(t, durableBaseline)
		jobsBaseline, err = buildOutboundJobBaseline(preEvalJobs, postEvalJobs, durableBaseline)
		if err != nil {
			t.Fatalf("bind email-eval outbound jobs: %v", err)
		}
		if err := validateInitialResponderRelationship(initialResponderResult, durableBaseline); err != nil {
			t.Fatal("initial responder result did not match the durable reply")
		}
		waitForDurableEvalState(t, pool, actor.UserID, actor.ID, target.ID, durableBaseline, jobsBaseline)
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
		waitForDurableEvalState(t, pool, actor.UserID, actor.ID, target.ID, durableBaseline, jobsBaseline)
		assertUnauthorizedDurableAbsence(t, pool, actor.UserID, target.ID, blockedKey)
		if after := forwarder.countRecipient(evalUnauthorizedAddress); after != unauthorizedEgressBaseline {
			t.Fatalf("unauthorized SMTP egress count changed from %d to %d", unauthorizedEgressBaseline, after)
		}
	})

	t.Run("stable reply idempotency has no duplicate or unauthorized SMTP egress", func(t *testing.T) {
		responder := startDeterministicResponder(t, ts.HTTPServer.URL, apiKey.PlaintextKey, target.EmailAddress())
		replayResult := responder.WaitForReply(t)
		if err := validateResponderReplayRelationship(initialResponderResult, durableBaseline, replayResult); err != nil {
			t.Fatalf("responder replay did not match the original durable outcome: %v", err)
		}
		waitForDurableEvalState(t, pool, actor.UserID, actor.ID, target.ID, durableBaseline, jobsBaseline)
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
