package client

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/dotcloud/docker/api"
	"github.com/dotcloud/docker/dockerversion"
	"github.com/dotcloud/docker/pkg/term"
	"github.com/dotcloud/docker/utils"
)

type ConnCloser interface {
	CloseWrite() error
	SetReadDeadline(t time.Time) error
}

type result struct {
	nr  int
	err error
}

type hijackedConn struct {
	conn         *ConnCloser
	reader       *bufio.Reader
	readyToRead  chan *[]byte
	readResult   chan result
	readyToClose chan bool
	closeResult  chan error
}

func (hc *hijackedConn) Read(p []byte) (n int, err error) {
	var (
		r result
	)
	hc.readyToRead <- &p
	r = <-hc.readResult
	return r.nr, r.err
}

func (hc *hijackedConn) CloseWrite() error {
	hc.readyToClose <- true
	return <-hc.closeResult
}

func (hc *hijackedConn) SetReadDeadline(t time.Time) error {
	return (*(hc.conn)).SetReadDeadline(t)
}

func NewHijackedConn(conn net.Conn, reader *bufio.Reader) *hijackedConn {
	var (
		hc     *hijackedConn
		closer ConnCloser
	)
	closer, ok := conn.(ConnCloser)
	if !ok {
		log.Fatal("failed to cast connection")
	}

	hc = &hijackedConn{&closer, reader, make(chan *[]byte, 1), make(chan result), make(chan bool), make(chan error)}
	go func(readyToRead chan *[]byte, readyToClose <-chan bool, readResult chan<- result, closeResult chan<- error) {
		var (
			eof    bool
			closed bool
			buffer *[]byte
		)
		eof = false
		closed = false

		for !eof || !closed {
			utils.Debugf("[sync] About to select")
			select {
			case buffer = <-readyToRead:
				utils.Debugf("[sync] About to read")
				if !closed {
					// once we have closed the write channel we can read with maximum efficiency
					hc.SetReadDeadline(time.Now().Add(time.Duration(500 * time.Millisecond)))
				} else {
					hc.SetReadDeadline(time.Unix(0, 0))
				}
				bytesRead, err := hc.reader.Read(*buffer)
				utils.Debugf("[sync] after read %d, %+v", bytesRead, err)
				eof = eof || err == io.EOF
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					if bytesRead == 0 {
						utils.Debugf("[sync] read timed out - trying again")
						readyToRead <- buffer
						continue
					} else {
						err = nil
					}
				}
				readResult <- result{bytesRead, err}
			case <-readyToClose:
				utils.Debugf("[sync] about to close")
				closeResult <- closer.CloseWrite()
				utils.Debugf("[sync] closed")
				closed = true
			}
		}

		utils.Debugf("[sync] about to exit")
	}(hc.readyToRead, hc.readyToClose, hc.readResult, hc.closeResult)
	return hc
}

func (cli *DockerCli) dial() (net.Conn, error) {
	if cli.tlsConfig != nil && cli.proto != "unix" {
		return tls.Dial(cli.proto, cli.addr, cli.tlsConfig)
	}
	return net.Dial(cli.proto, cli.addr)
}

func (cli *DockerCli) hijack(method, path string, setRawTerminal bool, in io.ReadCloser, stdout, stderr io.Writer, started chan io.Closer) error {
	defer func() {
		if started != nil {
			close(started)
		}
	}()

	req, err := http.NewRequest(method, fmt.Sprintf("/v%s%s", api.APIVERSION, path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Docker-Client/"+dockerversion.VERSION)
	req.Header.Set("Content-Type", "plain/text")
	req.Host = cli.addr

	dial, err := cli.dial()
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("Cannot connect to the Docker daemon. Is 'docker -d' running on this host?")
		}
		return err
	}
	clientconn := httputil.NewClientConn(dial, nil)
	defer clientconn.Close()

	// Server hijacks the connection, error 'connection closed' expected
	clientconn.Do(req)

	var hc *hijackedConn
	rwc, br := clientconn.Hijack()
	hc = NewHijackedConn(rwc, br)
	defer rwc.Close()

	if started != nil {
		started <- rwc
	}

	var receiveStdout chan error

	var oldState *term.State

	if in != nil && setRawTerminal && cli.isTerminal && os.Getenv("NORAW") == "" {
		oldState, err = term.SetRawTerminal(cli.terminalFd)
		if err != nil {
			return err
		}
		defer term.RestoreTerminal(cli.terminalFd, oldState)
	}

	if stdout != nil || stderr != nil {
		receiveStdout = utils.Go(func() (err error) {
			defer func() {
				if in != nil {
					if setRawTerminal && cli.isTerminal {
						term.RestoreTerminal(cli.terminalFd, oldState)
					}
					// For some reason this Close call blocks on darwin..
					// As the client exists right after, simply discard the close
					// until we find a better solution.
					if runtime.GOOS != "darwin" {
						in.Close()
					}
				}
			}()

			utils.Debugf("[hijack] about to copy")
			// When TTY is ON, use regular copy
			if setRawTerminal {
				_, err = io.Copy(stdout, br)
			} else {
				_, err = utils.StdCopy(stdout, stderr, hc)
			}
			utils.Debugf("[hijack] End of stdout")
			return err
		})
	}

	sendStdin := utils.Go(func() error {
		if in != nil {
			io.Copy(rwc, in)
			utils.Debugf("[hijack] End of stdin")
		}
		utils.Debugf("[hijack] About to close")
		hc.CloseWrite()
		utils.Debugf("[hijack] After close")
		// Discard errors due to pipe interruption
		return nil
	})

	if stdout != nil || stderr != nil {
		if err := <-receiveStdout; err != nil {
			utils.Debugf("Error receiveStdout: %s", err)
			return err
		}
	}

	if !cli.isTerminal {
		if err := <-sendStdin; err != nil {
			utils.Debugf("Error sendStdin: %s", err)
			return err
		}
	}
	return nil
}
