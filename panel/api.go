package main

import (
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type API struct {
	sup     *Supervisor
	console *Console
	col     *Collector
	players *Players
	files   *Files
	backups *Backups
	web     fs.FS
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /", http.FileServerFS(a.web))

	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("GET /api/console", a.consoleBacklog)
	mux.HandleFunc("GET /api/console/stream", a.consoleStream)
	mux.HandleFunc("POST /api/server/{action}", a.serverAction)
	mux.HandleFunc("POST /api/command", a.command)

	mux.HandleFunc("GET /api/players", a.playersView)
	mux.HandleFunc("POST /api/players/action", a.playersAction)
	mux.HandleFunc("POST /api/players/whitelist", a.whitelistToggle)

	mux.HandleFunc("GET /api/files", a.filesList)
	mux.HandleFunc("GET /api/file", a.fileRead)
	mux.HandleFunc("PUT /api/file", a.fileWrite)

	mux.HandleFunc("GET /api/backups", a.backupsList)
	mux.HandleFunc("POST /api/backups", a.backupCreate)
	mux.HandleFunc("DELETE /api/backups", a.backupDelete)
	mux.HandleFunc("GET /api/backups/download", a.backupDownload)

	return guard(mux)
}

// guard es la unica defensa que tiene sentido en un panel sin login: obliga a
// que las peticiones que cambian algo lleven una cabecera propia. El navegador
// no permite mandarla desde otro origen sin preflight, y el preflight falla
// porque no publicamos CORS. Sin esto, cualquier web abierta en el mismo
// portatil podria hacer POST /api/server/stop a 127.0.0.1.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			if !sameHost(origin, r.Host) {
				http.Error(w, "origen no permitido", http.StatusForbidden)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Panel") == "" {
				http.Error(w, "falta la cabecera X-Panel (peticion no hecha desde el panel)", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(origin, host string) bool {
	origin = strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	if origin == host {
		return true
	}
	oh, _, err1 := net.SplitHostPort(origin)
	hh, _, err2 := net.SplitHostPort(host)
	return err1 == nil && err2 == nil && oh == hh
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeOK(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusOK, map[string]string{"ok": msg})
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.col.Snapshot())
}

func (a *API) consoleBacklog(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	writeJSON(w, http.StatusOK, a.console.Snapshot(since))
}

// consoleStream sirve la consola por Server-Sent Events. Es una via de un solo
// sentido, que es justo lo que hace falta: los comandos van por POST y el
// navegador reconecta solo si se corta.
func (a *API) consoleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming no soportado", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	id, ch := a.console.Subscribe()
	defer a.console.Unsubscribe(id)

	send := func(l Line) bool {
		data, err := json.Marshal(l)
		if err != nil {
			return true
		}
		if _, err := w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
			return false
		}
		return true
	}

	for _, l := range a.console.Snapshot(since) {
		if !send(l) {
			return
		}
	}
	flusher.Flush()

	// Los comentarios SSE mantienen la conexion viva a traves de proxies que
	// cortan por inactividad.
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case l := <-ch:
			if !send(l) {
				return
			}
			// Vacia lo que haya llegado de golpe antes de hacer flush: durante
			// el arranque de Paper llegan cientos de lineas por segundo.
			drained := true
			for drained {
				select {
				case l2 := <-ch:
					if !send(l2) {
						return
					}
				default:
					drained = false
				}
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (a *API) serverAction(w http.ResponseWriter, r *http.Request) {
	var err error
	var msg string
	switch r.PathValue("action") {
	case "start":
		err, msg = a.sup.Start(), "arrancando"
	case "stop":
		err, msg = a.sup.Stop(), "parando"
	case "restart":
		err, msg = a.sup.Restart(), "reiniciando"
	case "kill":
		err, msg = a.sup.Kill(), "SIGKILL enviado"
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeOK(w, msg)
}

func (a *API) command(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cmd string `json:"cmd"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.sup.Send(body.Cmd); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	// Eco en la consola: Paper no repite los comandos que entran por stdin, y
	// sin esto no queda constancia de quien pidio que.
	a.console.Add(KindPanel, "> "+strings.TrimSpace(body.Cmd))
	writeOK(w, "enviado")
}

func (a *API) playersView(w http.ResponseWriter, r *http.Request) {
	v, err := a.players.View()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) playersAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
		Player string `json:"player"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	msg, err := a.players.Action(body.Action, body.Player, body.Reason)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeOK(w, msg)
}

func (a *API) whitelistToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	msg, err := a.players.SetWhitelistEnforced(body.On)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeOK(w, msg)
}

func (a *API) filesList(w http.ResponseWriter, r *http.Request) {
	list, err := a.files.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) fileRead(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	content, err := a.files.Read(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path, "content": content})
}

func (a *API) fileWrite(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	var body struct {
		Content string `json:"content"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	msg, err := a.files.Write(path, body.Content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	a.console.Add(KindPanel, "== panel: "+msg)
	// Aviso honesto: Paper lee la configuracion al arrancar, no en caliente.
	if a.sup.Status().State != StateStopped {
		msg += " - hara falta reiniciar el servidor para que surta efecto"
	}
	writeOK(w, msg)
}

func (a *API) backupsList(w http.ResponseWriter, r *http.Request) {
	list, err := a.backups.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) backupCreate(w http.ResponseWriter, r *http.Request) {
	if err := a.backups.Start(); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"ok": "backup en curso"})
}

func (a *API) backupDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.backups.Delete(r.URL.Query().Get("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeOK(w, "backup borrado")
}

func (a *API) backupDownload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	path, err := a.backups.Path(name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeFile(w, r, path)
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	// 4 MB cubre el fichero de configuracion mas grande que aceptamos editar.
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	return dec.Decode(v)
}
