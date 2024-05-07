package server

import (
	"encoding/json"
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

	jsonResp, err := json.Marshal(resp)
	if err != nil {
		log.Fatalf("error handling JSON marshal. Err: %v", err)
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
