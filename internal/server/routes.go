package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"golang.org/x/net/websocket"
)
func (s *Server) RegisterRoutes() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.HelloWorldHandler)

	mux.HandleFunc("/favicon.ico", s.DoNothingHandler)

	mux.HandleFunc("/health", s.healthHandler)

	mux.Handle("/ws", websocket.Handler(s.handleWS))

	s.routes = mux
}

func (s *Server) DoNothingHandler(w http.ResponseWriter, r *http.Request) {}

func (s *Server) HelloWorldHandler(w http.ResponseWriter, r *http.Request) {
	resp := make(map[string]string)
	resp["message"] = "Hello World"

	fmt.Println("test")
	jsonResp, err := json.Marshal(resp)
	if err != nil {
		log.Fatalf("error handling JSON marshal. Err: %v", err)
	}

	//TODO: remove this code, it was used for testing
	fmt.Println(s.conns)
	for conn := range s.conns {
		_, err := conn.Write(jsonResp)
		if err != nil {
			log.Fatalf("error writing to websocket. Err: %v", err)
		}
	}

	_, _ = w.Write(jsonResp)
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	jsonResp, err := json.Marshal(s.db.Health())

	if err != nil {
		log.Fatalf("error handling JSON marshal. Err: %v", err)
	}

	_, _ = w.Write(jsonResp)
}
