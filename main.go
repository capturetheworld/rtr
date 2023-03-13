package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/monoxane/nk"
)

var (
	router *nk.Router

	ProbeHandler             *ProbeSocketHandler
	MatrixWSConnections      = make(map[WebsocketConnection]uuid.UUID)
	MatrixWSConnectionsMutex sync.Mutex
	Upgrader                 = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
)

type WebsocketConnection struct {
	Mux    *sync.Mutex
	Socket *websocket.Conn
}

type MatrixWSMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type RouteRequest struct {
	Destination int `json:"destination"`
	Source      int `json:"source"`
}

type DestinationUpdate struct {
	Id     int          `json:"id"`
	Label  string       `json:"label"`
	Source SourceUpdate `json:"source"`
}

type SourceUpdate struct {
	Id    int    `json:"id"`
	Label string `json:"label"`
}

func main() {
	router = nk.New(Config.Router.IP, uint8(Config.Router.Address), Config.Router.Model)

	router.SetOnUpdate(func(d *nk.Destination) {
		log.Printf("Received update for %s, current source %s", d.Label, d.Source.Label)
		payload, _ := json.Marshal(DestinationUpdate{
			Id:    int(d.Id),
			Label: d.GetLabel(),
			Source: SourceUpdate{
				Id:    int(d.Source.Id),
				Label: d.Source.GetLabel(),
			},
		})
		update := MatrixWSMessage{
			Type: "destination_update",
			Data: payload,
		}

		MatrixWSConnectionsMutex.Lock()
		for conn := range MatrixWSConnections {
			conn.Socket.WriteJSON(update)
		}
		MatrixWSConnectionsMutex.Unlock()
	})

	data, err := os.ReadFile("labels.lbl")
	if err == nil {
		router.LoadLabels(string(data))
	} else {
		log.Printf("unable to load DashBoard labels.lbl")
	}

	if Config.Probe.Enabled {
		ProbeHandler = &ProbeSocketHandler{
			clients:    make(map[*ProbeClient]bool),
			register:   make(chan *ProbeClient),
			unregister: make(chan *ProbeClient),
			broadcast:  make(chan *[]byte),
			upgrader: &websocket.Upgrader{
				ReadBufferSize:  readBufferSize,
				WriteBufferSize: writeBufferSize,
				CheckOrigin: func(r *http.Request) bool {
					return true
				},
			},
		}

		go ProbeHandler.Run()
	}

	go router.Connect()
	go serveHTTP()

	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 1)

	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Println(sig)
		done <- true
	}()

	log.Println("Server Start Awaiting Signal")
	<-done
	log.Println("Exiting")
}

func serveHTTP() {
	gin.SetMode(gin.ReleaseMode)

	svc := gin.Default()
	svc.Use(CORSMiddleware())

	svc.StaticFile("/", "/dist/index.html")
	svc.NoRoute(func(c *gin.Context) {
		c.File("/dist/index.html")
	})
	svc.Static("/dist", "/dist")

	svc.GET("/v1/ws/matrix", HandleMatrixWS)

	svc.GET("/v1/matrix", HandleMatrix)

	svc.GET("/v1/config", HandleConfig)

	if Config.Probe.Enabled {
		svc.GET("/v1/ws/probe", ProbeHandler.ServeWS)
		svc.POST("/v1/probe/stream", HandleProbeStream)
	}

	err := svc.Run(fmt.Sprintf(":%d", Config.Server.Port))
	if err != nil {
		log.Fatalln("unable to start http server", err)
	}
}

func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept, Authorization, x-access-token")
		c.Header("Access-Control-Expose-Headers", "Content-Length, Access-Control-Allow-Origin, Access-Control-Allow-Headers, Cache-Control, Content-Language, Content-Type")
		c.Header("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

func HandleMatrix(c *gin.Context) {
	c.JSON(http.StatusOK, &router.Matrix)
}

func HandleConfig(c *gin.Context) {
	c.JSON(http.StatusOK, Config)
}

func HandleMatrixWS(c *gin.Context) {
	w := c.Writer
	r := c.Request

	// Process the upgrade request
	socket, err := Upgrader.Upgrade(w, r, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": http.StatusBadRequest, "message": "failed to upgrade ws connection", "error": err})

		return
	}

	connection := WebsocketConnection{
		Socket: socket,
		Mux:    new(sync.Mutex),
	}

	MatrixWSConnectionsMutex.Lock()
	MatrixWSConnections[connection] = uuid.Must(uuid.NewRandom())
	MatrixWSConnectionsMutex.Unlock()

	defer connection.Socket.Close()
	for {
		_, messageBytes, err := connection.Socket.ReadMessage()
		if err != nil {
			log.Printf("WSErr %s", err)
			MatrixWSConnectionsMutex.Lock()
			delete(MatrixWSConnections, connection)
			MatrixWSConnectionsMutex.Unlock()

			return
		}

		var message MatrixWSMessage
		unmarshalErr := json.Unmarshal(messageBytes, &message)
		if unmarshalErr != nil {
			log.Printf("unable to unmarshal Matrix WS Message %s", unmarshalErr)

			break
		}

		switch message.Type {
		case "route_request":
			var request RouteRequest
			unmarshalErr := json.Unmarshal(message.Data, &request)
			if unmarshalErr != nil {
				log.Printf("unable to unmarshal Route Request %s", unmarshalErr)
			}

			log.Printf("routing %d to %d", request.Source, request.Destination)

			router.Route(uint16(request.Destination), uint16(request.Source))
		}
	}
}

func HandleProbeStream(c *gin.Context) {
	log.Printf("IncomingStream connected: %s\n", c.RemoteIP())

	for {
		data, err := ioutil.ReadAll(io.LimitReader(c.Request.Body, 1024))
		if err != nil || len(data) == 0 {
			break
		}

		ProbeHandler.BroadcastData(&data)
	}

	log.Printf("IncomingStream disconnected: %s\n", c.RemoteIP())
}
