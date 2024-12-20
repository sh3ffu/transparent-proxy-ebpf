package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
)

type InterceptConfig struct {
	ConfigName     string          `json:"configName"`
	InterceptLinks []InterceptLink `json:"interceptLinks"`
}

type InterceptLink struct {
	Url       string `json:"url"`
	Intercept bool   `json:"intercept"`
}

var interceptConfig InterceptConfig = InterceptConfig{
	ConfigName:     "default",
	InterceptLinks: []InterceptLink{},
}

func initConfig() {
	file, err := os.Open("interceptLinks.json")
	if err != nil {
		fmt.Println("Error opening file:", err)
		return
	}
	defer file.Close()
	jsonBytes, _ := io.ReadAll(file)
	json.Unmarshal(jsonBytes, &interceptConfig)
	color.Yellow("Intercept config loaded: %v\n", interceptConfig)
}

/*
Debug function to print the request data to the console
*/
func printRequest(r io.Reader, id uint64, ready chan struct{}) {
	// Print the request data
	color.HiBlue("\nRequest %d data:\n", id)
	var buffer = make([]byte, 1024)
	for {
		n, err := r.Read(buffer)
		if err != nil {
			if err == io.EOF {
				break
			} else {
				log.Println("Print: Error reading request data:", err)
			}
			return
		}
		color.Cyan(string(buffer[:n]))
	}

	if ready != nil {
		ready <- struct{}{}
	}
}

/*
Debug function to print the response data to the console
*/
func printResponse(r io.Reader, response *http.Response, id uint64, ready chan struct{}) {
	// Print the response data
	color.HiBlue("\nResponse %d data:\n", id)

	//Print status line
	color.HiGreen("HTTP/1.1 " + response.Status + "\r\n")
	//Print headers
	for k, v := range response.Header {
		color.HiGreen(k + ": " + strings.Join(v, ", ") + "\r\n")
	}
	color.Green("\r\n")

	var buffer = make([]byte, 2048)
	for {
		n, err :=
			r.Read(buffer)
		if err != nil {
			if err == io.EOF {
				break
			} else {
				log.Println("Print: Error reading response data:", err)
			}
			return
		}
		color.Green(string(buffer[:n]))
	}

	if ready != nil {
		ready <- struct{}{}
	}
}

func handleHttpConn(conn net.Conn, targetAddr net.Addr) {

	defer conn.Close()
	// //Create mock http response:
	// response := "HTTP/1.1 200 OK\r\n" +
	// 	"Content-Type: text/html\r\n" +
	// 	"Content-Length: 11\r\n" +
	// 	"\r\n" +
	// 	"Hello World" +
	// 	"\r\n"

	// Increment the counter for each http request
	counter++

	ready := make(chan struct{})
	printRequestReady := make(chan struct{})
	handleResponseReady := make(chan struct{})

	pr, pw := io.Pipe()
	printTee := io.TeeReader(conn, pw)

	go printRequest(printTee, counter, printRequestReady)

	go func() {
		defer pr.Close()
		defer pw.Close()

		// Read the request data
		request, err := parseHttpRequest(pr)
		if err != nil {
			log.Printf("HandleHttp: Failed to parse HTTP request: %v", err)
			return
		}
		fixRequest(request)

		if shouldIntercept(request) {
			// Intercept the request
			// Send the response to the client

			log.Printf("Intercepting request %d", counter)

			response, err := forwardHttpRequest(*request)
			if err != nil {
				log.Printf("Failed to forward http request to server: %v", err)
				return
			} else {
				log.Printf("Attempting to forward response to client")

				// Print the response data
				handleResponse(response, conn, counter)
			}
		} else {
			// Request is not of interest, connect the client to the server directly

			log.Printf("Ignoring requuest %d", counter)
			connectClientToServerDirectly(conn, targetAddr, request)
		}

		ready <- struct{}{}
	}()

	<-ready
	<-printRequestReady
	<-handleResponseReady

}

func handleResponse(response *http.Response, conn net.Conn, id uint64) {
	defer response.Body.Close()
	//defer conn.Close()

	// mockResponse := "HTTP/1.1 200 OK\r\n" +
	// 	"Content-Type: text/html\r\n" +
	// 	"Content-Length: 11\r\n" +
	// 	"\r\n" +
	// 	"Hello World" +
	// 	"\r\n"

	conn.Write([]byte("HTTP/1.1 " + response.Status + "\r\n"))

	for k, v := range response.Header {
		conn.Write([]byte(k + ": " + strings.Join(v, ", ") + "\r\n"))
	}
	conn.Write([]byte("\r\n"))

	// Print the response data and forward it to the client

	// Create a multiwriter to write the response to the client and to the console
	consolePipeReader, consolePipeWriter := io.Pipe()
	defer consolePipeReader.Close()
	defer consolePipeWriter.Close()

	multiWriter := io.MultiWriter(conn, consolePipeWriter)

	printReady := make(chan struct{})
	go printResponse(consolePipeReader, response, id, printReady)
	_, err := io.Copy(multiWriter, response.Body)
	// uncomment for mock response
	//_, err := io.Copy(multiWriter, strings.NewReader(mockResponse))
	conn.Close()

	if err != nil {
		log.Printf("Failed to forward response body to client: %v", err)
		return
	}
	<-printReady

}

func parseHttpRequest(r io.Reader) (*http.Request, error) {
	// Parse the HTTP request data
	request, err := http.ReadRequest(bufio.NewReader(r))
	if err != nil {
		return nil, err
	}
	return request, nil
}

func shouldIntercept(request *http.Request) bool {
	for _, link := range interceptConfig.InterceptLinks {
		if !link.Intercept {
			continue
		}
		if strings.Contains(request.URL.String(), link.Url) {
			fmt.Printf("Intercepting request to %s\n", link.Url)
			return true
		}
	}
	//TODO: change logic here after fixing the intercept config
	return false
}

/*
The following function is used to fix the URL and URI fields of the request
so that the request can be forwarded correctly
*/
func fixRequest(request *http.Request) {
	//remove the request URI
	request.RequestURI = ""

	// Fix the URL field
	host := request.Host
	path := request.URL.Path

	newURL, err := url.Parse(fmt.Sprintf("http://%s%s", host, path))
	if err != nil {
		log.Printf("fixUrl: error parsing url %v", err)
	}
	request.URL = newURL
}

func forwardHttpRequest(request http.Request) (*http.Response, error) {

	res, err := http.DefaultClient.Do(&request)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// /* Writes a response to the provided connection */
// func writeResponseToConn(response http.Response, conn net.Conn) {
// 	// Write the response to the client
// 	conn.Write([]byte("HTTP/1.1 " + response.Status + "\r\n"))
// 	for k, v := range response.Header {
// 		conn.Write([]byte(k + ": " + strings.Join(v, ", ") + "\r\n"))
// 	}
// 	conn.Write([]byte("\r\n"))
// 	_, err := io.Copy(conn, response.Body)
// 	if err != nil {
// 		log.Printf("Failed to write response data to conn: %v", err)
// 	}
// }

/* Connects the client to the server directly */
func connectClientToServerDirectly(conn net.Conn, targetAddr net.Addr, request *http.Request) {
	targetConn, err := net.DialTimeout("tcp", targetAddr.String(), 5*time.Second)
	if err != nil {
		log.Printf("HandleHttp: Failed to connect to original destination: %v", err)
		return
	}

	// Close the connection to the target server when the function returns
	defer targetConn.Close()
	defer conn.Close()

	// Forward the processed request to the target
	// The following code creates two data transfer channels:
	// - From the client to the target server (handled by a separate goroutine).
	// - From the target server to the client (handled by the main goroutine).
	ready := make(chan struct{})
	go func() {
		err := request.Write(targetConn)
		if err != nil {
			log.Printf("Failed to forward connection to target: %v", err)
			return
		}
		ready <- struct{}{}
	}()

	_, err = io.Copy(conn, targetConn)
	if err != nil {
		log.Printf("Failed to forward connection from target: %v", err)
		return
	}
	<-ready
}
