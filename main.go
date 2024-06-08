package main

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -type Config proxy proxy.c

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

const (
	CGROUP_PATH = "/sys/fs/cgroup" // Root cgroup path
	PROXY_PORT      = 18000 // Port where the proxy server listens
	SO_ORIGINAL_DST = 80 // Socket option to get the original destination address
)

// SockAddrIn is a struct to hold the sockaddr_in structure for IPv4 "retrieved" by the SO_ORIGINAL_DST.
type SockAddrIn struct {
	SinFamily uint16
	SinPort   [2]byte
	SinAddr   [4]byte
	// Pad to match the size of sockaddr_in
	Pad [8]byte
}

// helper function for getsockopt
func getsockopt(s int, level int, optname int, optval unsafe.Pointer, optlen *uint32) (err error) {
	_, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(s), uintptr(level), uintptr(optname), uintptr(optval), uintptr(unsafe.Pointer(optlen)), 0)
	if e != 0 {
		return e
	}
	return
}

// HTTP proxy request handler
func handleConnection(conn net.Conn) {
	defer conn.Close()

	// Using RawConn is necessary to perform low-level operations on the underlying socket file descriptor in Go. 
	// This allows us to use getsockopt to retrieve the original destination address set by the SO_ORIGINAL_DST option, 
	// which isn't directly accessible through Go's higher-level networking API.
	rawConn, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		log.Printf("Failed to get raw connection: %v", err)
		return
	}

	var originalDst SockAddrIn
	// If Control is not nil, it is called after creating the network connection but before binding it to the operating system.
	rawConn.Control(func(fd uintptr) {
		optlen := uint32(unsafe.Sizeof(originalDst))
		// Retrieve the original destination address by making a syscall with the SO_ORIGINAL_DST option.
		err = getsockopt(int(fd), syscall.SOL_IP, SO_ORIGINAL_DST, unsafe.Pointer(&originalDst), &optlen)
		if err != nil {
			log.Printf("getsockopt SO_ORIGINAL_DST failed: %v", err)
		}
	})

	targetAddr := net.IPv4(originalDst.SinAddr[0], originalDst.SinAddr[1], originalDst.SinAddr[2], originalDst.SinAddr[3]).String()
	targetPort := (uint16(originalDst.SinPort[0]) << 8) | uint16(originalDst.SinPort[1])

	fmt.Printf("Original destination: %s:%d\n", targetAddr, targetPort)

	// Check that the original destination address is reachable from the proxy
	targetConn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", targetAddr, targetPort), 5*time.Second)
	if err != nil {
		log.Printf("Failed to connect to original destination: %v", err)
		return
	}
	defer targetConn.Close()

	fmt.Printf("Proxying connection from %s to %s\n", conn.RemoteAddr(), targetConn.RemoteAddr())

	// The following code creates two data transfer channels:
  // - From the client to the target server (handled by a separate goroutine).
  // - From the target server to the client (handled by the main goroutine).
	go func() {
		_, err = io.Copy(targetConn, conn)
		if err != nil {
			log.Printf("Failed copying data to target: %v", err)
		}
	}()
	_, err = io.Copy(conn, targetConn)
	if err != nil {
		log.Printf("Failed copying data from target: %v", err)
	}
}

func main() {
	// Remove resource limits for kernels <5.11.
	if err := rlimit.RemoveMemlock(); err != nil { 
		log.Print("Removing memlock:", err)
	}

	// Load the compiled eBPF ELF and load it into the kernel 
	// NOTE: we could also pin the eBPF program
	var objs proxyObjects
	if err := loadProxyObjects(&objs, nil); err != nil {
			log.Print("Error loading eBPF objects:", err)
	}
	defer objs.Close()

	// Attach eBPF programs to the root cgroup
	connect4Link, err := link.AttachCgroup(link.CgroupOptions{
		Path:    CGROUP_PATH, 
		Attach:  ebpf.AttachCGroupInet4Connect,
		Program: objs.CgConnect4,
	})
	if err != nil {
			log.Print("Attaching CgConnect4 program to Cgroup:", err)
	}
	defer connect4Link.Close() 

	sockopsLink, err := link.AttachCgroup(link.CgroupOptions{
		Path:    CGROUP_PATH,
		Attach:  ebpf.AttachCGroupSockOps,
		Program: objs.CgSockOps,
	})
	if err != nil {
			log.Print("Attaching CgSockOps program to Cgroup:", err)
	}
	defer sockopsLink.Close() 

	sockoptLink, err := link.AttachCgroup(link.CgroupOptions{
		Path:    CGROUP_PATH,
		Attach:  ebpf.AttachCGroupGetsockopt,
		Program: objs.CgSockOpt,
	})
	if err != nil {
			log.Print("Attaching CgSockOpt program to Cgroup:", err)
	}
	defer sockoptLink.Close() 

	// Start the proxy server on the localhost
	// We only demonstrate IPv4 in this example, but the same approach can be used for IPv6
	proxyAddr := fmt.Sprintf("127.0.0.1:%d", PROXY_PORT)
	listener, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		log.Fatalf("Failed to start proxy server: %v", err)
	}
	defer listener.Close()

	// Update the proxyMaps map with the proxy server configuration, because we need to know the proxy server PID in order
	// to filter out eBPF events generated by the proxy server itself so it would not proxy its own packets in a loop.
	var key uint32 = 0
	config := proxyConfig{
		ProxyPort: PROXY_PORT,
		ProxyPid: uint64(os.Getpid()),
	}
	err = objs.proxyMaps.MapConfig.Update(&key, &config, ebpf.UpdateAny)
	if err != nil {
		log.Fatalf("Failed to update proxyMaps map: %v", err)
	}

	log.Printf("Proxy server with PID %d listening on %s", os.Getpid(), proxyAddr)
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		go handleConnection(conn)
	}
}