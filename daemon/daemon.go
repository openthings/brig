package daemon

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/VividCortex/godaemon"
	"github.com/disorganizer/brig/daemon/proto"
	"github.com/disorganizer/brig/repo"
	protobuf "github.com/gogo/protobuf/proto"
)

///////////////////////
// UTILITY FUNCTIONS //
///////////////////////

// send transports a msg over conn with a size header.
func send(conn net.Conn, msg protobuf.Message) error {
	data, err := protobuf.Marshal(msg)
	if err != nil {
		return nil
	}

	sizeBuf := make([]byte, binary.MaxVarintLen64)
	binary.PutUvarint(sizeBuf, uint64(len(data)))

	n, err := conn.Write(sizeBuf)
	if err != nil {
		return err
	}

	if n < len(sizeBuf) {
		return io.ErrShortWrite
	}

	n, err = conn.Write(data)
	if err != nil {
		return err
	}

	if n < len(data) {
		return io.ErrShortWrite
	}

	return nil
}

// recv reads a size-prefixed protobuf buffer
func recv(conn net.Conn, msg protobuf.Message) error {
	sizeBuf := make([]byte, binary.MaxVarintLen64)
	n, err := conn.Read(sizeBuf)
	if err != nil {
		return err
	}

	size, _ := binary.Uvarint(sizeBuf[:n])
	if size > 1*1024*1024 {
		return fmt.Errorf("Message too large: %d", size)
	}

	buf := make([]byte, size)
	n, err = conn.Read(buf)
	if err != nil {
		return err
	}

	err = protobuf.Unmarshal(buf, msg)
	if err != nil {
		return err
	}

	return nil
}

//////////////////////////
// FRONTEND DAEMON PART //
//////////////////////////

// Daemon is the top-level struct of the brig daemon.
type DaemonClient struct {
	// Use this channel to send commands to the daemon
	Send chan *proto.Command

	// Responses are sent to this channel
	Recv chan *proto.Response

	// Underlying tcp connection:
	conn net.Conn

	// Be able to tell handleMessages to stop
	quit chan bool
}

// Dial connects to a running daemon instance.
func Dial(port int) (*DaemonClient, error) {
	client := &DaemonClient{
		Send: make(chan *proto.Command),
		Recv: make(chan *proto.Response),
		quit: make(chan bool, 1),
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	client.conn = conn

	go client.handleMessages()
	return client, nil
}

// handleMessages takes all messages from the Send channel
// and actually sends them over the network. It then waits
// for the response and puts it in the Recv channel.
func (c *DaemonClient) handleMessages() {
	for {
		select {
		case <-c.quit:
			return
		case msg := <-c.Send:
			if err := send(c.conn, msg); err != nil {
				log.Warning("client-send: ", err)
				c.Recv <- nil
				continue
			}

			resp := &proto.Response{}
			if err := recv(c.conn, resp); err != nil {
				log.Warning("client-recv: ", err)
				c.Recv <- nil
				continue
			}

			c.Recv <- resp
		}
	}
}

// Reach tries to Dial() the daemon, if not there it Launch()'es one.
func Reach(repoPath string, port int) (*DaemonClient, error) {
	// Try to Dial directly first:
	if daemon, err := Dial(port); err == nil {
		return daemon, nil
	}

	// Probably not running, find out our own binary:
	exePath, err := godaemon.GetExecutablePath()
	if err != nil {
		return nil, err
	}

	// Start a new daemon process:
	log.Info("Starting daemon: ", exePath)
	proc, err := os.StartProcess(
		exePath, []string{"brig", "daemon"}, &os.ProcAttr{},
	)

	if err != nil {
		return nil, err
	}

	// Make sure it it's still referenced:
	go func() {
		log.Info("Daemon has PID: ", proc.Pid)
		if _, err := proc.Wait(); err != nil {
			log.Warning("Bad exit state: ", err)
		}
	}()

	// Wait at max 5 seconds for the daemon to start up:
	// (this means, wait till it's network interface is started)
	for i := 0; i < 5; i++ {
		time.Sleep(1 * time.Second)
		client, err := Dial(port)
		if err != nil {
			return nil, err
		}

		if client != nil {
			return client, nil
		}
	}

	return nil, fmt.Errorf("Daemon could not be started or took to long.")
}

// Ping returns true if the daemon is running and responds correctly.
func (c *DaemonClient) Ping() bool {
	cmd := &proto.Command{}
	cmd.CommandType = proto.MessageType_PING.Enum()

	c.Send <- cmd
	resp := <-c.Recv
	if resp != nil {
		return "PONG" == resp.GetResponse()
	}

	return false
}

// Exorcise sends a QUIT message to the daemon.
func (c *DaemonClient) Exorcise() {
	cmd := &proto.Command{}
	cmd.CommandType = proto.MessageType_QUIT.Enum()
	c.Send <- cmd
}

// Close shuts down the daemon client
func (c *DaemonClient) Close() {
	if c != nil {
		c.quit <- true
	}
}

// LocalAddr() returns a net.Addr with the client end of the Connection
func (c *DaemonClient) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr() returns a net.Addr with the server end of the Connection
func (c *DaemonClient) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

/////////////////////////
// BACKEND DAEMON PART //
/////////////////////////

// DaemonServer is a TCP server that executed all commands
// on a single repository.
type DaemonServer struct {
	// The repo we're working on
	Repo *repo.FsRepository

	done    chan bool
	signals chan os.Signal

	// TCP Listener for incoming connections:
	listener net.Listener
}

// Summon creates a new up and running DaemonServer instance
func Summon(port int) (*DaemonServer, error) {
	// Listen for incoming connections.
	addr := fmt.Sprintf("localhost:%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("Error listening:", err.Error())
		return nil, err
	}

	// Close the listener when the application closes.
	log.Info("Listening on ", addr)

	daemon := &DaemonServer{
		done:     make(chan bool, 1),
		signals:  make(chan os.Signal, 1),
		listener: listener,
	}

	// Daemon mainloop:
	go func() {
		// Forward signals to the quit channel:
		signal.Notify(daemon.signals, os.Interrupt, os.Kill)

		for {
			select {
			case <-daemon.signals:
				// Break the Serve() loop
				daemon.done <- true
				return
			default:
				// Listen for an incoming connection.
				deadline := time.Now().Add(500 * time.Millisecond)
				err := listener.(*net.TCPListener).SetDeadline(deadline)
				if err != nil {
					break
				}

				conn, err := listener.Accept()
				if err != nil && err.(*net.OpError).Timeout() {
					continue
				}

				if err != nil {
					log.Errorf("Error accepting: %v", err.Error())
					break
				}

				// Handle connections in a new goroutine.
				go daemon.handleRequest(conn)
			}
		}
	}()

	return daemon, nil
}

// Serve waits until the DaemonServer received a quit event.
func (d *DaemonServer) Serve() {
	<-d.done
	d.listener.Close()
}

// Handles incoming requests:
func (d *DaemonServer) handleRequest(conn net.Conn) {
	defer conn.Close()
	for {
		msg := &proto.Command{}
		if err := recv(conn, msg); err != nil {
			log.Warning("daemon recv: ", err)
			return
		}

		d.handleCommand(msg, conn)
	}
}

// Handles the actual incoming commands:
func (d *DaemonServer) handleCommand(cmd *proto.Command, conn net.Conn) {
	log.Info("Processing message: ", cmd)

	// Prepare a response template
	resp := &proto.Response{}
	resp.ResponseType = cmd.CommandType

	switch *(cmd.CommandType) {
	case proto.MessageType_INIT:
		fmt.Println("Init")
	case proto.MessageType_ADD:
	case proto.MessageType_CAT:
	case proto.MessageType_QUIT:
		d.signals <- os.Interrupt
		resp.Response = protobuf.String("BYE")
	case proto.MessageType_PING:
		resp.Response = protobuf.String("PONG")
	default:
		fmt.Println("Unknown message type.")
		return
	}

	if err := send(conn, resp); err != nil {
		log.Warning("Unable to send message back to client: ", err)
	}
}
