package server

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/net/websocket"
)

func (s *Server) handleWS(ws *websocket.Conn) {
	fmt.Println("new incoming connection for client:", ws.RemoteAddr())

	if ws.Request().Host != os.Getenv("APP_URL") {
		ws.Close()
		return
	}

	fmt.Println("connection accepted")

	if !s.conns[ws] {
		s.conns[ws] = true
	}
	
	defer func() {
		fmt.Println("closing connection for client:", ws.RemoteAddr())
		delete(s.conns, ws)
		ws.Close()
	}()

	s.readLoop(ws)
}

func (s *Server) readLoop(ws *websocket.Conn) {
	buf := make([]byte, 1024)
	for {
		n, err := ws.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Println("read error:", err)
			continue
		}

		msg := buf[:n]

		fmt.Println("received msg:", string(msg))
	}
}


