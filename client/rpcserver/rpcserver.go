// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package rpcserver provides a JSON RPC to communicate with the client core.
package rpcserver

import (
	"context"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sync"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/websocket"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"github.com/decred/dcrd/certgen"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const (
	// rpcTimeoutSeconds is the number of seconds a connection to the
	// RPC server is allowed to stay open without authenticating before it
	// is closed.
	rpcTimeoutSeconds = 10

	// RPC version. Move major up one for breaking changes. Move minor for
	// backwards compatible features. Move patch for bug fixes.
	rpcSemverMajor = 0
	rpcSemverMinor = 1
	rpcSemverPatch = 0
)

var (
	// Check that core.Core satisfies clientCore.
	_   clientCore = (*core.Core)(nil)
	log dex.Logger
	// errUnknownCmd is wrapped when the command is not know.
	errUnknownCmd = errors.New("unknown command")
)

// clientCore is satisfied by core.Core.
type clientCore interface {
	websocket.Core
	AssetBalance(assetID uint32) (*core.WalletBalance, error)
	Book(host string, base, quote uint32) (orderBook *core.OrderBook, err error)
	Cancel(appPass []byte, orderID dex.Bytes) error
	CloseWallet(assetID uint32) error
	CreateWallet(appPass, walletPass []byte, form *core.WalletForm) error
	Exchanges() (exchanges map[string]*core.Exchange)
	InitializeClient(appPass, seed []byte) error
	Login(appPass []byte) (*core.LoginResult, error)
	Logout() error
	OpenWallet(assetID uint32, appPass []byte) error
	GetFee(addr string, cert interface{}) (fee uint64, err error)
	Register(form *core.RegisterForm) (*core.RegisterResult, error)
	Trade(appPass []byte, form *core.TradeForm) (order *core.Order, err error)
	Wallets() (walletsStates []*core.WalletState)
	WalletState(assetID uint32) *core.WalletState
	Withdraw(appPass []byte, assetID uint32, value uint64, addr string) (asset.Coin, error)
	ExportSeed(pw []byte) ([]byte, error)
}

// RPCServer is a single-client http and websocket server enabling a JSON
// interface to the DEX client.
type RPCServer struct {
	core      clientCore
	mux       *chi.Mux
	wsServer  *websocket.Server
	addr      string
	tlsConfig *tls.Config
	srv       *http.Server
	authSHA   [32]byte
	wg        sync.WaitGroup
}

// genCertPair generates a key/cert pair to the paths provided.
func genCertPair(certFile, keyFile string, hosts []string) error {
	log.Infof("Generating TLS certificates...")

	org := "dcrdex autogenerated cert"
	validUntil := time.Now().Add(10 * 365 * 24 * time.Hour)
	cert, key, err := certgen.NewTLSCertPair(elliptic.P521(), org,
		validUntil, hosts)
	if err != nil {
		return err
	}

	// Write cert and key files.
	if err = ioutil.WriteFile(certFile, cert, 0644); err != nil {
		return err
	}
	if err = ioutil.WriteFile(keyFile, key, 0600); err != nil {
		os.Remove(certFile)
		return err
	}

	log.Infof("Done generating TLS certificates")
	return nil
}

// writeJSON marshals the provided interface and writes the bytes to the
// ResponseWriter. The response code is assumed to be StatusOK.
func writeJSON(w http.ResponseWriter, thing interface{}) {
	writeJSONWithStatus(w, thing, http.StatusOK)
}

// writeJSONWithStatus marshals the provided interface and writes the bytes to the
// ResponseWriter with the specified response code.
func writeJSONWithStatus(w http.ResponseWriter, thing interface{}, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	b, err := json.Marshal(thing)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Errorf("JSON encode error: %v", err)
		return
	}
	w.WriteHeader(code)
	_, err = w.Write(b)
	if err != nil {
		log.Errorf("Write error: %v", err)
	}
}

// handleJSON handles all https json requests.
func (s *RPCServer) handleJSON(w http.ResponseWriter, r *http.Request) {
	// All http routes are available over websocket too, so do not support
	// persistent http connections. Inform the user and close the connection
	// when response handling is completed.
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Type", "application/json")
	r.Close = true

	body, err := ioutil.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, "error reading request body", http.StatusBadRequest)
		return
	}
	req := new(msgjson.Message)
	err = json.Unmarshal(body, req)
	if err != nil {
		http.Error(w, "JSON decode error", http.StatusUnprocessableEntity)
		return
	}
	if req.Type != msgjson.Request {
		http.Error(w, "Responses not accepted", http.StatusMethodNotAllowed)
		return
	}
	s.parseHTTPRequest(w, req)
}

// Config holds variables neede to create a new RPC Server.
type Config struct {
	Core                        clientCore
	Addr, User, Pass, Cert, Key string
	CertHosts                   []string
}

// SetLogger sets the logger for the RPCServer package.
func SetLogger(logger dex.Logger) {
	log = logger
}

// New is the constructor for an RPCServer.
func New(cfg *Config) (*RPCServer, error) {

	if cfg.Pass == "" {
		return nil, fmt.Errorf("missing RPC password")
	}

	// Find or create the key pair.
	keyExists := fileExists(cfg.Key)
	certExists := fileExists(cfg.Cert)
	if certExists == !keyExists {
		return nil, fmt.Errorf("missing cert pair file")
	}
	if !keyExists && !certExists {
		err := genCertPair(cfg.Cert, cfg.Key, cfg.CertHosts)
		if err != nil {
			return nil, err
		}
	}
	keypair, err := tls.LoadX509KeyPair(cfg.Cert, cfg.Key)
	if err != nil {
		return nil, err
	}

	// Prepare the TLS configuration.
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{keypair},
		MinVersion:   tls.VersionTLS12,
	}

	// Create an HTTP router.
	mux := chi.NewRouter()
	httpServer := &http.Server{
		Handler:      mux,
		ReadTimeout:  rpcTimeoutSeconds * time.Second, // slow requests should not hold connections opened
		WriteTimeout: rpcTimeoutSeconds * time.Second, // hung responses must die
	}

	// Make the server.
	s := &RPCServer{
		core:      cfg.Core,
		mux:       mux,
		srv:       httpServer,
		addr:      cfg.Addr,
		tlsConfig: tlsConfig,
		wsServer:  websocket.New(cfg.Core, log.SubLogger("WS")),
	}

	// Create authSHA to verify requests against.
	login := cfg.User + ":" + cfg.Pass
	auth := "Basic " +
		base64.StdEncoding.EncodeToString([]byte(login))
	s.authSHA = sha256.Sum256([]byte(auth))

	// Middleware
	mux.Use(middleware.Recoverer)
	mux.Use(middleware.RealIP)
	mux.Use(s.authMiddleware)

	// The WebSocket handler is mounted on /ws in Connect.

	// HTTPS endpoint
	mux.Post("/", s.handleJSON)

	return s, nil
}

// Connect starts the RPC server. Satisfies the dex.Connector interface.
func (s *RPCServer) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	// Create listener.
	listener, err := tls.Listen("tcp", s.addr, s.tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("can't listen on %s. rpc server quitting: %w", s.addr, err)
	}
	// Update the listening address in case a :0 was provided.
	s.addr = listener.Addr().String()

	// Close the listener on context cancellation.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-ctx.Done()

		if err := s.srv.Shutdown(context.Background()); err != nil {
			// Error from closing listeners:
			log.Errorf("HTTP server Shutdown: %v", err)
		}
	}()

	// Configure the websocket handler before starting the server.
	s.mux.Get("/ws", func(w http.ResponseWriter, r *http.Request) {
		s.wsServer.HandleConnect(ctx, w, r)
	})

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.srv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			log.Warnf("unexpected (http.Server).Serve error: %v", err)
		}
		// Disconnect the websocket clients since http.(*Server).Shutdown does
		// not deal with hijacked websocket connections.
		s.wsServer.Shutdown()
		log.Infof("RPC server off")
	}()
	log.Infof("RPC server listening on %s", s.addr)
	return &s.wg, nil
}

// handleRequest sends the request to the correct handler function if able.
func (s *RPCServer) handleRequest(req *msgjson.Message) *msgjson.ResponsePayload {
	payload := new(msgjson.ResponsePayload)
	if req.Route == "" {
		log.Debugf("route not specified")
		payload.Error = msgjson.NewError(msgjson.RPCUnknownRoute, "no route was supplied")
		return payload
	}

	// Find the correct handler for this route.
	h, exists := routes[req.Route]
	if !exists {
		log.Debugf("%v: %v", errUnknownCmd, req.Route)
		payload.Error = msgjson.NewError(msgjson.RPCUnknownRoute, errUnknownCmd.Error())
		return payload
	}

	params := new(RawParams)
	err := req.Unmarshal(params) // NOT &params to prevent setting it to nil for []byte("null") Payload
	if err != nil {
		log.Debugf("cannot unmarshal params for route %s", req.Route)
		payload.Error = msgjson.NewError(msgjson.RPCParseError, "unable to unmarshal request")
		return payload
	}

	return h(s, params)
}

// parseHTTPRequest parses the msgjson message in the request body, creates a
// response message, and writes it to the http.ResponseWriter.
func (s *RPCServer) parseHTTPRequest(w http.ResponseWriter, req *msgjson.Message) {
	payload := s.handleRequest(req)
	resp, err := msgjson.NewResponse(req.ID, payload.Result, payload.Error)
	if err != nil {
		msg := fmt.Sprintf("error encoding response: %v", err)
		http.Error(w, msg, http.StatusInternalServerError)
		log.Errorf("parseHTTPRequest: NewResponse failed: %s", msg)
		return
	}
	writeJSON(w, resp)
}

// authMiddleware checks incoming requests for authentication.
func (s *RPCServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func() {
			log.Warnf("authentication failure from ip: %s", r.RemoteAddr)
			w.Header().Add("WWW-Authenticate", `Basic realm="dex RPC"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		}
		auth := r.Header["Authorization"]
		if len(auth) == 0 {
			fail()
			return
		}
		authSHA := sha256.Sum256([]byte(auth[0]))
		if subtle.ConstantTimeCompare(s.authSHA[:], authSHA[:]) != 1 {
			fail()
			return
		}
		log.Debugf("authenticated user with ip: %s", r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

// filesExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	_, err := os.Stat(name)
	return !os.IsNotExist(err)
}
