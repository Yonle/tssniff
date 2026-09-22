package main

import (
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/pojntfx/go-nbd/pkg/server"
)

// NBDServer wraps the go-nbd server and manages the external nbd-client
// process that attaches the kernel NBD device to it.
type NBDServer struct {
	backend   *NBDBackend
	sockPath  string
	listener  net.Listener
	clientCmd *exec.Cmd
	devPath   string
}

func NewNBDServer(backend *NBDBackend, devPath string) *NBDServer {
	return &NBDServer{
		backend:  backend,
		sockPath: filepath.Join("/tmp", "tsdisk-nbd.sock"),
		devPath:  devPath,
	}
}

// Start begins listening and spawns nbd-client to attach /dev/nbd0.
func (s *NBDServer) Start() error {
	// Remove stale socket.
	os.Remove(s.sockPath)

	l, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return err
	}
	s.listener = l

	// Accept loop in the background.
	go s.acceptLoop()

	// Attach the kernel device via the external nbd-client.
	return s.connectClient()
}

func (s *NBDServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			if err := server.Handle(
				conn,
				[]*server.Export{
					{
						Name:        "default",
						Description: "TSSniff virtual disk",
						Backend:     s.backend,
					},
				},
				&server.Options{
					ReadOnly:           false,
					MinimumBlockSize:   512,
					PreferredBlockSize: 512,
					MaximumBlockSize:   512,
				},
			); err != nil {
				log.Printf("NBD server handle error: %v", err)
			}
		}()
	}
}

// connectClient invokes nbd-client to attach the kernel NBD device.
func (s *NBDServer) connectClient() error {
	// Give the socket a moment to be ready.
	time.Sleep(200 * time.Millisecond)

	size, _ := s.backend.Size()
	log.Printf("Connecting nbd-client to %s (%d bytes)", s.devPath, size)

	cmd := exec.Command("nbd-client",
		"-unix", s.sockPath,
		"-block-size", "512",
		"-N", "default",
		s.devPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	s.clientCmd = cmd
	log.Printf("NBD device %s ready", s.devPath)
	return nil
}

// Stop disconnects the NBD device and closes the listener.
func (s *NBDServer) Stop() {
	if s.clientCmd != nil {
		_ = exec.Command("nbd-client", "-d", s.devPath).Run()
	}
	if s.listener != nil {
		s.listener.Close()
	}
	os.Remove(s.sockPath)
}