package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed web
var webFiles embed.FS

func main() {
	log.SetFlags(log.Ltime)

	addr := flag.String("addr", "127.0.0.1:8080", "direccion donde escucha el panel")
	serverDir := flag.String("server", "", "directorio del servidor (por defecto se busca ./server hacia arriba)")
	backupDir := flag.String("backups", "", "directorio de backups (por defecto <raiz>/backups)")
	memMin := flag.String("mem-min", "2G", "MEM_MIN que se pasa a start.sh")
	memMax := flag.String("mem-max", "4G", "MEM_MAX que se pasa a start.sh")
	autostart := flag.Bool("autostart", false, "arrancar el servidor de Minecraft al abrir el panel")
	mirror := flag.Bool("mirror", true, "repetir la consola del servidor en la salida del panel")
	flag.Parse()

	dir, err := resolveServerDir(*serverDir)
	if err != nil {
		log.Fatalf("no encuentro el servidor: %v", err)
	}

	root := filepath.Dir(dir)
	backups := *backupDir
	if backups == "" {
		// Fuera de server/ a proposito: deploy.sh sincroniza ese directorio
		// con --delete y se llevaria los backups por delante.
		backups = filepath.Join(root, "backups")
	}

	console := NewConsole()
	if *mirror {
		// Mantiene el comportamiento de antes: con el panel bajo systemd, la
		// consola de Paper sigue apareciendo en journalctl -u lukybox.
		console.OnLine(func(l Line) { fmt.Println(l.Text) })
	}

	sup, err := NewSupervisor(dir, *memMin, *memMax, console)
	if err != nil {
		log.Fatal(err)
	}
	bk, err := NewBackups(backups, dir, sup, console)
	if err != nil {
		log.Fatalf("no puedo usar %s para backups: %v", backups, err)
	}
	col := NewCollector(sup, console, bk, dir, backups)

	api := &API{
		sup:     sup,
		console: console,
		col:     col,
		players: NewPlayers(dir, sup),
		files:   NewFiles(dir),
		backups: bk,
	}
	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}
	api.web = sub

	stopCollector := make(chan struct{})
	go col.Run(stopCollector)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout se queda a cero: la consola por SSE es una respuesta que
		// no termina nunca y cualquier limite la cortaria.
		IdleTimeout: 120 * time.Second,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("no puedo escuchar en %s: %v", *addr, err)
	}

	log.Printf("Lucky Box panel")
	log.Printf("  servidor : %s", dir)
	log.Printf("  backups  : %s", backups)
	log.Printf("  RAM      : %s - %s", *memMin, *memMax)
	log.Printf("  abre     : http://%s", *addr)
	if !isLoopback(*addr) {
		log.Printf("  AVISO: %s no es local y el panel NO tiene contraseña.", *addr)
		log.Printf("         Cualquiera que llegue a ese puerto puede parar el servidor")
		log.Printf("         y editar la configuracion. Usalo solo detras de un tunel SSH.")
	}
	if pid := ForeignServerPID(dir, 0); pid != 0 {
		log.Printf("  AVISO: ya hay un servidor corriendo en ese directorio (pid %d)", pid)
		log.Printf("         y no lo ha arrancado el panel: no podra pararlo ni verlo.")
	}

	if *autostart {
		if err := sup.Start(); err != nil {
			log.Printf("autostart fallido: %v", err)
		}
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("servidor http: %v", err)
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	log.Printf("recibido %s: cerrando", sig)

	close(stopCollector)

	// Primero el mundo, luego el HTTP. Si saliesemos sin parar la JVM se
	// quedaria huerfana y el mundo sin guardar.
	if sup.Status().State != StateStopped {
		log.Printf("parando el servidor de Minecraft (Paper esta guardando el mundo)...")
		sup.StopWait()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	log.Printf("listo")
}

// resolveServerDir localiza el directorio del servidor. Con el binario dentro
// del repo lo normal es lanzarlo desde cualquier sitio, asi que se busca
// server/start.sh subiendo desde el directorio actual y desde el del binario.
func resolveServerDir(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(abs, "start.sh")); err != nil {
			return "", fmt.Errorf("%s no contiene start.sh", abs)
		}
		return abs, nil
	}

	var starts []string
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}

	for _, start := range starts {
		dir := start
		for i := 0; i < 5; i++ {
			cand := filepath.Join(dir, "server")
			if _, err := os.Stat(filepath.Join(cand, "start.sh")); err == nil {
				return cand, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", errors.New("no hay ningun server/start.sh cerca; pasa -server /ruta/al/server")
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
