package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	ALL    = "/all"    // /all               lists all channels on the server
	CREATE = "/create" // /create #mychan    creates new channel #mychan, no auto-join
	JOIN   = "/join"   // /join #mychan      joins newly created channel
	LIST   = "/list"   // /list #mychan      lists all connected clients for #mychan
	LEAVE  = "/leave"  // /leave #mychan     client leaves #mychan
	MSG    = "/msg"    // /msg #mychan text  sends text to all participants of #mychan
	MY     = "/my"     // /my                lists all channels of client
	QUIT   = "/quit"   // /quit              leaves all channels and disconnects client
)

func main() {
	address := flag.String("address", "localhost:8888", "Listening address for TCP connections")
	acceptor := flag.Int("acceptor", 10, "Number of TCP connection acceptors")
	flag.Parse()
	Listen(*address, *acceptor)
}

type Server struct {
	connected    chan *client
	disconnected chan string
	messages     chan *message
	shutdown     chan struct{}
}

func newServer() *Server {
	return &Server{
		connected:    make(chan *client),
		disconnected: make(chan string),
		messages:     make(chan *message),
		shutdown:     make(chan struct{}),
	}
}

func Listen(address string, acceptor int) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return
	}
	log.Println("Listening on", address)
	defer ln.Close()
	s := newServer()
	s.accept(ln, acceptor)
	s.handleConnections()
}

func (s *Server) accept(ln net.Listener, num int) {
	for i := 0; i < num; i++ {
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					close(s.shutdown)
				}
				log.Println("New client ", conn.RemoteAddr())
				client := newClient(s, conn)
				go client.connected()
			}
		}()
	}
}

func (s *Server) handleConnections() {
	public := "#public"
	clients := make(map[string]*client)
	channels := make(map[string]*channel)
	channels[public] = newChannel(public)
	stats := newStatistics()
	stats.every(time.Second)
	go channels[public].handleActivity(stats)

	defer func() {
		sysmsg := fmt.Sprintf("Server shutting down!\n")
		for _, client := range clients {
			client.write <- sysmsg
			client.Close()
		}
	}()

	for {
		select {
		case client := <-s.connected:
			if _, exists := clients[client.username]; exists {
				fmt.Fprintf(client, "Name %s is not available\n", client.username)
				go client.connected()
				continue
			}
			go client.handleMessages()
			clients[client.username] = client
			sysmsg := fmt.Sprintf("Connected to chatserver. Welcome %s!\n", client.username)
			clients[client.username].write <- sysmsg
			channels[public].join <- client
		case msg := <-s.messages:
			switch msg.command {
			case JOIN:
				if channel, exists := channels[msg.channel]; exists {
					channel.join <- clients[msg.sender]
				} else {
					sysmsg := fmt.Sprintf("Channel %s does not exist\n", msg.channel)
					clients[msg.sender].write <- sysmsg
				}
			case CREATE:
				if _, exists := channels[msg.channel]; !exists {
					channels[msg.channel] = newChannel(msg.channel)
					go channels[msg.channel].handleActivity(stats)
					sysmsg := fmt.Sprintf("New channel %s has been created\n", msg.channel)
					clients[msg.sender].write <- sysmsg
				} else {
					sysmsg := fmt.Sprintf("Channel %s already exists\n", msg.channel)
					clients[msg.sender].write <- sysmsg
				}
			case ALL:
				keys := make([]string, len(channels))
				i := 0
				for k := range channels {
					keys[i] = k
					i++
				}
				clients[msg.sender].write <- strings.Join(keys, "\n") + "\n"
			}
		case username := <-s.disconnected:
			clients[username].Close()
			delete(clients, username)
		case <-s.shutdown:
			return
		}
	}
}

type channel struct {
	name     string
	join     chan *client
	leave    chan string
	list     chan string
	messages chan *message
}

func newChannel(name string) *channel {
	return &channel{
		name:     name,
		join:     make(chan *client),
		leave:    make(chan string),
		list:     make(chan string),
		messages: make(chan *message),
	}
}

func (ch *channel) handleActivity(stats *statistics) {
	clients := make(map[string]*client)
	var prefix = func(t time.Time) string {
		return t.Format("Mon Jan 2 15:04") + " | " + ch.name + " | "
	}
	var broadcast = func(msg string) {
		for _, client := range clients {
			client.write <- msg
		}
	}

	for {
		select {
		case msg := <-ch.messages:
			broadcast(prefix(msg.time) + msg.sender + " > " + msg.text + "\n")
		case client := <-ch.join:
			clients[client.username] = client
			client.joined <- ch
			sysmsg := fmt.Sprintf("%s has joined the channel\n", client.username)
			broadcast(prefix(time.Now()) + sysmsg)
		case username := <-ch.leave:
			client := clients[username]
			delete(clients, username)
			client.left <- ch.name
			sysmsg := fmt.Sprintf("%s has left the channel\n", username)
			broadcast(prefix(time.Now()) + sysmsg)
		case username := <-ch.list:
			keys := make([]string, len(clients))
			i := 0
			for k := range clients {
				keys[i] = k
				i++
			}
			clients[username].write <- strings.Join(keys, "\n") + "\n"
		}
		stats.messageProcessed()
	}
}

type client struct {
	*bufio.Reader
	net.Conn
	server   *Server
	joined   chan *channel
	left     chan string
	write    chan string
	username string
}

func newClient(s *Server, c net.Conn) *client {
	return &client{
		Reader: bufio.NewReader(c),
		Conn:   c,
		server: s,
		joined: make(chan *channel),
		left:   make(chan string),
		write:  make(chan string),
	}
}

func (c *client) connected() {
	for c.username = ""; c.username == ""; {
		fmt.Fprint(c, "Enter username: ")
		name, err := c.ReadString('\n')
		if err != nil {
			c.Close()
			return
		}
		c.username = strings.TrimSpace(name)
	}
	c.server.connected <- c
}

func (c *client) handleMessages() {
	channels := make(map[string]*channel)
	read, readerror := readLoop(c)
	writeerror := writeLoop(c)

	defer func() {
		done := make(chan struct{})
		go func() {
			for {
				select {
				case <-c.write:
				case <-done:
					return
				}
			}
		}()
		for _, channel := range channels {
			channel.leave <- c.username
			<-c.left
		}
		done <- struct{}{}
		close(c.write)
		c.server.disconnected <- c.username
	}()

	for {
		select {
		case channel := <-c.joined:
			channels[channel.name] = channel
		case name := <-c.left:
			delete(channels, name)
		case str := <-read:
			msg := newMessage(c.username, str)
			switch msg.command {
			case CREATE, JOIN, ALL:
				c.server.messages <- msg
			case MSG:
				if _, exists := channels[msg.channel]; exists {
					channels[msg.channel].messages <- msg
				} else {
					c.write <- fmt.Sprintf("Not part of channel %s\n", msg.channel)
				}
			case LIST:
				if _, exists := channels[msg.channel]; exists {
					channels[msg.channel].list <- c.username
				} else {
					c.write <- fmt.Sprintf("Not part of channel %s\n", msg.channel)
				}
			case LEAVE:
				if _, exists := channels[msg.channel]; exists {
					channels[msg.channel].leave <- c.username
					delete(channels, msg.channel)
					c.write <- fmt.Sprintf("Leaving channel %s ...\n", msg.channel)
				} else {
					c.write <- fmt.Sprintf("Not part of channel %s\n", msg.channel)
				}
			case MY:
				keys := make([]string, len(channels))
				i := 0
				for k := range channels {
					keys[i] = k
					i++
				}
				c.write <- strings.Join(keys, "\n") + "\n"
			case QUIT:
				c.write <- fmt.Sprintf("Disconnecting %s\n", msg.sender)
				return
			default:
				c.write <- fmt.Sprintf("Invalid command %s\n", msg.command)
			}
		case <-readerror:
			return
		case <-writeerror:
			return
		}
	}
}

func readLoop(c *client) (chan string, chan struct{}) {
	read := make(chan string)
	readerror := make(chan struct{})
	go func() {
		defer close(read)
		for {
			str, err := c.ReadString('\n')
			if err != nil {
				readerror <- struct{}{}
				return
			}
			read <- str
		}
	}()
	return read, readerror
}

func writeLoop(c *client) chan struct{} {
	writeerror := make(chan struct{})
	go func() {
		for str := range c.write {
			c.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
			if _, err := c.Write([]byte(str)); err != nil {
				writeerror <- struct{}{}
				return
			}
		}
	}()
	return writeerror
}

type message struct {
	time    time.Time
	sender  string
	command string
	channel string
	text    string
}

func newMessage(sender string, str string) *message {
	tokens := strings.Split(strings.TrimSpace(str), " ")
	switch len(tokens) {
	case 1:
		return &message{time.Now(), sender, tokens[0], "", ""}
	case 2:
		return &message{time.Now(), sender, tokens[0], tokens[1], ""}
	default:
		return &message{time.Now(), sender, tokens[0], tokens[1], strings.Join(tokens[2:], " ")}
	}
}

type statistics struct {
	count int
	lock  sync.Mutex
}

func newStatistics() *statistics {
	var m sync.Mutex
	return &statistics{0, m}
}

func (s *statistics) messageProcessed() {
	s.lock.Lock()
	s.count++
	s.lock.Unlock()
}

func (s *statistics) every(t time.Duration) {
	var current int
	go func() {
		for {
			<-time.Tick(t)
			s.lock.Lock()
			current = s.count
			s.count = 0
			s.lock.Unlock()
			if current > 0 {
				log.Println(current)
			}
		}
	}()
}
