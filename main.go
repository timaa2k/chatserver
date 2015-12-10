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
	addr := flag.String("addr", "localhost:8888", "listen address")
	flag.Parse()
	log.Fatal(Listen(*addr))
}

type Server struct {
	connected    chan *client
	disconnected chan string
	messages     chan *message
	shutdown     chan bool
}

func newServer() *Server {
	return &Server{
		connected:    make(chan *client),
		disconnected: make(chan string),
		messages:     make(chan *message),
		shutdown:     make(chan bool),
	}
}

func Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Println("Listening on", addr)
	defer ln.Close()
	s := newServer()
	go s.handleConnections()
	for {
		conn, err := ln.Accept()
		if err != nil {
			close(s.shutdown)
			return err
		}
		log.Println("New client ", conn.RemoteAddr())
		client := newClient(s, conn)
		go client.connected()
	}
}

func (s *Server) handleConnections() {
	public := "#public"
	clients := make(map[string]*client)
	channels := make(map[string]*channel)
	channels[public] = newChannel(public)
	stat := statchan()
	go channels[public].handleActivity(stat)

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
			clients[client.username] = client
			go client.writeLoop()
			go client.readLoop()
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
					go channels[msg.channel].handleActivity(stat)
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
	clients  map[string]*client
	join     chan *client
	leave    chan string
	list     chan string
	messages chan *message
}

func newChannel(name string) *channel {
	return &channel{
		name:     name,
		clients:  make(map[string]*client),
		join:     make(chan *client),
		leave:    make(chan string),
		list:     make(chan string),
		messages: make(chan *message),
	}
}

func (ch *channel) handleActivity(stat chan struct{}) {
	var prefix = func(t time.Time) string {
		return t.Format("Mon Jan 2 15:04") + " | " + ch.name + " | "
	}
	var broadcast = func(msg string) {
		for _, client := range ch.clients {
			client.write <- msg
		}
	}
	for {
		select {
		case msg := <-ch.messages:
			broadcast(prefix(msg.time) + msg.sender + " > " + msg.text + "\n")
			stat <- struct{}{}
		case client := <-ch.join:
			ch.clients[client.username] = client
			client.mapmutex.Lock()
			client.channels[ch.name] = ch
			client.mapmutex.Unlock()
			sysmsg := fmt.Sprintf("%s has joined the channel\n", client.username)
			broadcast(prefix(time.Now()) + sysmsg)
			stat <- struct{}{}
		case username := <-ch.list:
			keys := make([]string, len(ch.clients))
			i := 0
			for k := range ch.clients {
				keys[i] = k
				i++
			}
			ch.clients[username].write <- strings.Join(keys, "\n") + "\n"
			stat <- struct{}{}
		case username := <-ch.leave:
			delete(ch.clients, username)
			sysmsg := fmt.Sprintf("%s has left the channel\n", username)
			broadcast(prefix(time.Now()) + sysmsg)
			stat <- struct{}{}
		}
	}
}

type client struct {
	*bufio.Reader
	net.Conn
	server   *Server
	channels map[string]*channel
	mapmutex sync.RWMutex
	write    chan string
	done     chan struct{}
	username string
}

func newClient(s *Server, c net.Conn) *client {
	var mutex sync.RWMutex
	return &client{
		Reader:   bufio.NewReader(c),
		Conn:     c,
		server:   s,
		channels: make(map[string]*channel),
		mapmutex: mutex,
		write:    make(chan string),
		done:     make(chan struct{}),
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

func (c *client) readLoop() {
	proxy := make(chan *message, 2)
	for {
		str, err := c.ReadString('\n')
		if err != nil {
			c.done <- struct{}{}
			c.disconnect()
			return
		}
		proxy <- newMessage(c.username, str)
		select {
		case <-c.done:
			return
		case msg := <-proxy:
			switch msg.command {
			case CREATE, JOIN, ALL:
				c.server.messages <- msg
			case MSG:
				c.mapmutex.RLock()
				if _, exists := c.channels[msg.channel]; exists {
					c.channels[msg.channel].messages <- msg
				} else {
					c.write <- fmt.Sprintf("Not part of channel %s\n", msg.channel)
				}
				c.mapmutex.RUnlock()
			case LIST:
				c.mapmutex.RLock()
				if _, exists := c.channels[msg.channel]; exists {
					c.channels[msg.channel].list <- c.username
				} else {
					c.write <- fmt.Sprintf("Not part of channel %s\n", msg.channel)
				}
				c.mapmutex.RUnlock()
			case LEAVE:
				c.mapmutex.RLock()
				if _, exists := c.channels[msg.channel]; exists {
					c.write <- fmt.Sprintf("Leaving channel %s ...\n", msg.channel)
					c.channels[msg.channel].leave <- c.username
				} else {
					c.write <- fmt.Sprintf("Not part of channel %s\n", msg.channel)
				}
				c.mapmutex.RUnlock()
			case MY:
				c.mapmutex.RLock()
				keys := make([]string, len(c.channels))
				i := 0
				for k := range c.channels {
					keys[i] = k
					i++
				}
				c.write <- strings.Join(keys, "\n") + "\n"
				c.mapmutex.RUnlock()
			case QUIT:
				c.write <- fmt.Sprintf("Disconnecting %s\n", msg.sender)
				c.disconnect()
			default:
				c.write <- fmt.Sprintf("Invalid command %s\n", msg.command)
			}
		}
	}
}

func (c *client) writeLoop() {
	for msg := range c.write {
		c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := c.Write([]byte(msg)); err != nil {
			c.done <- struct{}{}
			go func() {
				for _ = range c.write {
				}
			}()
			c.disconnect()
			break
		}
	}
}

func (c *client) disconnect() {
	go func() {
		c.mapmutex.Lock()
		for _, channel := range c.channels {
			channel.leave <- c.username
			delete(c.channels, channel.name)
		}
		c.mapmutex.Unlock()
		c.server.disconnected <- c.username
	}()
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

func statchan() chan struct{} {
	in := make(chan struct{})
	cnt := 0
	go func() {
		for {
			select {
			case <-time.Tick(time.Second):
				log.Println(time.Now(), cnt)
				cnt = 0
			case <-in:
				cnt++
			}
		}
	}()
	return in
}
