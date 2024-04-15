package server

import (
	"fmt"
	"io"

	"golang.org/x/net/websocket"
)

func (s *Server) handleWS(ws *websocket.Conn) {
	fmt.Println("new incoming connection for client:", ws.RemoteAddr())

	s.conns[ws] = true

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

		msg := buf[n]
		fmt.Println(string(msg))

		ws.Write([]byte("Thank you for the msg!"))
	}

}


