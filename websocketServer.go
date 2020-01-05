package signalr

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"golang.org/x/net/websocket"
	"net/http"
)

// MapHub used to register a SignalR Hub with the specified ServeMux
func MapHub(mux *http.ServeMux, path string, hubProto HubInterface) *Server {
	mux.HandleFunc(fmt.Sprintf("%s/negotiate", path), negotiateHandler)
	server, _ := NewServer(SimpleTransientHubFactory(hubProto))
	mux.Handle(path, websocket.Handler(func(ws *websocket.Conn) {
		connectionID := ws.Request().URL.Query().Get("id")
		if len(connectionID) == 0 {
			// Support websocket connection without negotiate
			connectionID = getConnectionID()
		}
		server.Run(&webSocketConnection{ws, nil, connectionID})
	}))
	return server
}

func negotiateHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		w.WriteHeader(400)
		return
	}

	connectionID := getConnectionID()

	response := negotiateResponse{
		ConnectionID: connectionID,
		AvailableTransports: []availableTransport{
			{
				Transport:       "WebSockets",
				TransferFormats: []string{"Text", "Binary"},
			},
		},
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		fmt.Println(err)
	}
}

func getConnectionID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		fmt.Println(err)
	}
	return base64.StdEncoding.EncodeToString(bytes)
}

type availableTransport struct {
	Transport       string   `json:"transport"`
	TransferFormats []string `json:"transferFormats"`
}

type negotiateResponse struct {
	ConnectionID        string               `json:"connectionId"`
	AvailableTransports []availableTransport `json:"availableTransports"`
}
