package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"log/syslog"
	"os"
	"runtime"
	"sync"
	"syscall"

	"github.com/kavu/go_reuseport"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	// Startup flags
	listenAddress  = kingpin.Flag("listen", "Address and port to listen on").Required().TCP()
	forwardAddress = kingpin.Flag("target", "Address to foward connections to").Required().TCP()
	clientNames    = kingpin.Flag("client", "Expected client organizational unit name (can be repeated)").Required().Strings()
	privateKeyPath = kingpin.Flag("key", "Path to private key file (PEM/PKCS1)").Required().String()
	certChainPath  = kingpin.Flag("cert", "Path to certificate chain file (PEM/X509)").Required().String()
	caBundlePath   = kingpin.Flag("cacert", "Path to certificate authority bundle file (PEM/X509)").Required().String()
	useSyslog      = kingpin.Flag("syslog", "Send logs to syslog instead of stderr").Bool()

	// Internal flags for reload
	gracefulChild = kingpin.Flag("graceful", "Send SIGTERM to parent after startup (internal)").Bool()
)

// Global logger instance
var logger *log.Logger

func initLogger() {
	if *useSyslog {
		var err error
		logger, err = syslog.NewLogger(syslog.LOG_NOTICE|syslog.LOG_DAEMON, log.LstdFlags|log.Lmicroseconds)
		panicOnError(err)
	} else {
		logger = log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	}

	// Set log prefix to process ID to distinguish parent/child
	logger.SetPrefix(fmt.Sprintf("[%5d] ", os.Getpid()))
}

// panicOnError panics if err is not nil
func panicOnError(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	kingpin.Parse()
	initLogger()

	// Open listening socket. Take note that we create a "reusable port
	// listener", meaning we pass SO_REUSEPORT to the kernel. This allows
	// us to have multiple processes listening on the same port and accept
	// connections. This is useful for the purposes of replacing certificates
	// in-place without having to take downtime, e.g. if a certificate is
	// expiring. See also reexec().
	network, address := decodeAddress(*listenAddress)
	rawListener, err := reuseport.NewReusablePortListener(network, address)
	panicOnError(err)

	// Wrap listening socket with TLS listener.
	listener := tls.NewListener(rawListener, buildConfig())
	logger.Printf("listening on %s", *listenAddress)

	wg := &sync.WaitGroup{}
	wg.Add(1)

	// A channel to allow signal handlers to notify our main accept loop
	// that it must shut down.
	stopper := make(chan bool, 1)

	go accept(listener, wg, stopper)
	go sigtermHandler(listener, stopper)
	go sigusr1Handler()

	// Are we a child process spawned by a reloading parent? Send SIGTERM to
	// parent to indicate successful startup.
	if *gracefulChild {
		parent := syscall.Getppid()
		logger.Printf("sending SIGTERM to parent PID %d", parent)
		syscall.Kill(parent, syscall.SIGTERM)
	}

	logger.Printf("startup completed, waiting for connections")

	wg.Wait()

	logger.Printf("all connections closed, shutting down")
}
