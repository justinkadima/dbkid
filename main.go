package main

import (
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func loopbackListenAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", errors.New("listen address must be a loopback host and port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return "", errors.New("listen address must have a valid port")
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1" // Never resolve a hostname to a non-loopback address.
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", errors.New("listen address must be loopback")
	}
	return net.JoinHostPort(host, strconv.Itoa(number)), nil
}

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "loopback address for the HTTP server")
	flag.Parse()
	listenAddress, err := loopbackListenAddress(*address)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("DBKid listening at http://%s", listener.Addr())
	server := &http.Server{
		Handler:           newServer(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	log.Fatal(server.Serve(listener))
}
