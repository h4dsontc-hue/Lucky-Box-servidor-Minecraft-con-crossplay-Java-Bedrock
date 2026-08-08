package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
)

// Paper guarda el mundo al recibir "stop". lukybox.service da 90s de margen
// para mundos grandes, asi que el panel usa la misma escala antes de insistir.
const (
	stopGraceTimeout = 90 * time.Second
	sigtermTimeout   = 30 * time.Second
	restartPause     = 3 * time.Second
)

var (
	reDone = regexp.MustCompile(`Done \([0-9.]+s\)!`)
	reJoin = regexp.MustCompile(`\]: ([A-Za-z0-9_.]{1,32}) joined the game`)
	reLeft = regexp.MustCompile(`\]: ([A-Za-z0-9_.]{1,32}) left the game`)

	// Paper colorea la consola con ANSI y Minecraft usa § para el color del
	// chat. Los quitamos al leer: no aportan nada en HTML y romperian el
	// parseo de la respuesta de "tps".
	reANSI    = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")
	reSection = regexp.MustCompile("§.")
)

type Player struct {
	Name    string `json:"name"`
	Bedrock bool   `json:"bedrock"`
	Since   string `json:"since"`
}

// Supervisor arranca server/start.sh como proceso hijo y se queda con sus
// pipes. Eso es lo que permite mandar comandos por stdin sin habilitar RCON.
// start.sh termina en `exec java`, asi que el PID del hijo es la JVM: las
// señales y stdin llegan directas a Paper, sin shell intermedia.
type Supervisor struct {
	dir     string
	script  string
	memMin  string
	memMax  string
	console *Console

	mu        sync.Mutex
	state     State
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	pid       int
	startedAt time.Time
	ready     bool
	lastExit  string
	done      chan struct{}
	online    map[string]time.Time
	busy      bool // hay una parada o reinicio en curso
}

func NewSupervisor(dir, memMin, memMax string, console *Console) (*Supervisor, error) {
	script := filepath.Join(dir, "start.sh")
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("no encuentro el script de arranque %s: %w", script, err)
	}
	s := &Supervisor{
		dir:     dir,
		script:  script,
		memMin:  memMin,
		memMax:  memMax,
		console: console,
		state:   StateStopped,
		online:  make(map[string]time.Time),
	}
	console.OnLine(s.observe)
	return s, nil
}

// observe lee el estado del servidor de su propia salida: no hay otra forma de
// saber cuando Paper ha terminado de cargar ni quien esta conectado.
func (s *Supervisor) observe(l Line) {
	if l.Kind == KindPanel {
		return
	}
	if reDone.MatchString(l.Text) {
		s.mu.Lock()
		s.ready = true
		if s.state == StateStarting {
			s.state = StateRunning
		}
		s.mu.Unlock()
		return
	}
	if m := reJoin.FindStringSubmatch(l.Text); m != nil {
		s.mu.Lock()
		s.online[m[1]] = time.Now()
		s.mu.Unlock()
		return
	}
	if m := reLeft.FindStringSubmatch(l.Text); m != nil {
		s.mu.Lock()
		delete(s.online, m[1])
		s.mu.Unlock()
	}
}

func (s *Supervisor) Start() error {
	s.mu.Lock()
	if s.state != StateStopped {
		st := s.state
		s.mu.Unlock()
		return fmt.Errorf("el servidor esta %s", st)
	}
	if s.busy {
		s.mu.Unlock()
		return errors.New("hay una transicion en curso, espera")
	}

	cmd := exec.Command("bash", s.script)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "MEM_MIN="+s.memMin, "MEM_MAX="+s.memMax)
	// Grupo de procesos propio: si la JVM se cuelga podemos señalar al arbol
	// completo con kill(-pgid) en vez de dejar huerfanos.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("no pude arrancar %s: %w", s.script, err)
	}

	s.cmd, s.stdin, s.pid = cmd, stdin, cmd.Process.Pid
	s.state = StateStarting
	s.startedAt = time.Now()
	s.ready = false
	s.lastExit = ""
	s.online = make(map[string]time.Time)
	done := make(chan struct{})
	s.done = done
	pid := s.pid
	s.mu.Unlock()

	s.console.Add(KindPanel, fmt.Sprintf("== panel: arrancando start.sh (pid %d, RAM %s-%s)", pid, s.memMin, s.memMax))

	var wg sync.WaitGroup
	wg.Add(2)
	go s.pump(&wg, stdout, KindOut)
	go s.pump(&wg, stderr, KindErr)

	go func() {
		wg.Wait() // los pipes se cierran cuando muere la JVM
		werr := cmd.Wait()

		s.mu.Lock()
		s.state = StateStopped
		s.cmd, s.stdin, s.pid = nil, nil, 0
		s.ready = false
		s.online = make(map[string]time.Time)
		if werr != nil {
			s.lastExit = werr.Error()
		} else {
			s.lastExit = "salida limpia"
		}
		msg := s.lastExit
		close(done)
		s.mu.Unlock()

		s.console.Add(KindPanel, "== panel: el servidor se ha detenido ("+msg+")")
	}()
	return nil
}

func (s *Supervisor) pump(wg *sync.WaitGroup, r io.Reader, kind Kind) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	// Los crash reports y los volcados de FAWE traen lineas larguisimas; el
	// limite por defecto de 64KB las cortaria a mitad.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		text := strings.TrimRight(sc.Text(), "\r")
		text = reANSI.ReplaceAllString(text, "")
		text = reSection.ReplaceAllString(text, "")
		s.console.Add(kind, text)
	}
}

// Send escribe un comando en stdin de la JVM, tal cual lo escribirias en la
// consola del servidor.
func (s *Supervisor) Send(cmd string) error {
	cmd = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmd), "/"))
	if cmd == "" {
		return errors.New("comando vacio")
	}
	if strings.ContainsAny(cmd, "\n\r") {
		return errors.New("el comando no puede tener saltos de linea")
	}
	s.mu.Lock()
	stdin := s.stdin
	s.mu.Unlock()
	if stdin == nil {
		return errors.New("el servidor no esta arrancado")
	}
	if _, err := io.WriteString(stdin, cmd+"\n"); err != nil {
		return fmt.Errorf("no pude escribir en la consola: %w", err)
	}
	return nil
}

// Stop pide la parada ordenada y vuelve en seguida: el navegador ve el estado
// "stopping" mientras Paper guarda el mundo.
func (s *Supervisor) Stop() error {
	done, err := s.beginStop()
	if err != nil {
		return err
	}
	go s.waitStop(done)
	return nil
}

// StopWait para el servidor y espera. Lo usa el apagado del propio panel.
func (s *Supervisor) StopWait() {
	done, err := s.beginStop()
	if err != nil {
		return
	}
	s.waitStop(done)
}

func (s *Supervisor) beginStop() (chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == StateStopped {
		return nil, errors.New("el servidor ya esta parado")
	}
	if s.busy {
		return nil, errors.New("ya hay una parada en curso")
	}
	s.busy = true
	s.state = StateStopping
	return s.done, nil
}

func (s *Supervisor) waitStop(done chan struct{}) {
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	s.mu.Lock()
	stdin, pid := s.stdin, s.pid
	s.mu.Unlock()

	s.console.Add(KindPanel, "== panel: enviando 'stop', Paper esta guardando el mundo")
	if stdin != nil {
		io.WriteString(stdin, "stop\n")
	}
	select {
	case <-done:
		return
	case <-time.After(stopGraceTimeout):
	}

	s.console.Add(KindPanel, "== panel: no responde, enviando SIGTERM al grupo de procesos")
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGTERM)
	}
	select {
	case <-done:
		return
	case <-time.After(sigtermTimeout):
	}

	s.console.Add(KindPanel, "== panel: sigue vivo, enviando SIGKILL (el mundo puede quedar sin guardar)")
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
	<-done
}

func (s *Supervisor) Restart() error {
	s.mu.Lock()
	st, busy, done := s.state, s.busy, s.done
	s.mu.Unlock()

	if busy {
		return errors.New("hay una transicion en curso, espera")
	}
	if st == StateStopped {
		return s.Start()
	}
	if err := s.Stop(); err != nil {
		return err
	}
	go func() {
		<-done
		time.Sleep(restartPause) // deja que el puerto 25565 quede libre
		if err := s.Start(); err != nil {
			s.console.Add(KindPanel, "== panel: fallo al rearrancar: "+err.Error())
		}
	}()
	return nil
}

// Kill es el ultimo recurso: mata el arbol sin dar tiempo a guardar.
func (s *Supervisor) Kill() error {
	s.mu.Lock()
	pid := s.pid
	s.mu.Unlock()
	if pid <= 0 {
		return errors.New("el servidor no esta arrancado")
	}
	s.console.Add(KindPanel, "== panel: SIGKILL forzado por el usuario")
	return syscall.Kill(-pid, syscall.SIGKILL)
}

type SupervisorStatus struct {
	State     State    `json:"state"`
	PID       int      `json:"pid"`
	Ready     bool     `json:"ready"`
	StartedAt string   `json:"startedAt,omitempty"`
	UptimeSec int64    `json:"uptimeSec"`
	LastExit  string   `json:"lastExit,omitempty"`
	Players   []Player `json:"players"`
	MemMin    string   `json:"memMin"`
	MemMax    string   `json:"memMax"`
	Busy      bool     `json:"busy"`
}

func (s *Supervisor) Status() SupervisorStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := SupervisorStatus{
		State:    s.state,
		PID:      s.pid,
		Ready:    s.ready,
		LastExit: s.lastExit,
		MemMin:   s.memMin,
		MemMax:   s.memMax,
		Busy:     s.busy,
		Players:  make([]Player, 0, len(s.online)),
	}
	if !s.startedAt.IsZero() && s.state != StateStopped {
		st.StartedAt = s.startedAt.Format(time.RFC3339)
		st.UptimeSec = int64(time.Since(s.startedAt).Seconds())
	}
	for name, since := range s.online {
		st.Players = append(st.Players, Player{
			Name: name,
			// Floodgate prefija con "." los nombres de Bedrock: es la unica
			// pista que da el log de la edicion del jugador.
			Bedrock: strings.HasPrefix(name, "."),
			Since:   since.Format(time.RFC3339),
		})
	}
	sort.Slice(st.Players, func(i, j int) bool { return st.Players[i].Name < st.Players[j].Name })
	return st
}

func (s *Supervisor) IsReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready && s.state == StateRunning
}

func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

// ForeignServerPID busca una JVM de este mismo servidor que el panel no haya
// arrancado (alguien lanzo ./start.sh a mano, o hay dos paneles abiertos).
// Sin esto el usuario veria "parado" con el servidor corriendo de verdad.
func ForeignServerPID(serverDir string, ownPID int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	want := []byte("paper.jar")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := 0
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid == 0 || pid == ownPID {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || !containsBytes(cmdline, want) {
			continue
		}
		cwd, err := os.Readlink("/proc/" + e.Name() + "/cwd")
		if err != nil || cwd != serverDir {
			continue
		}
		return pid
	}
	return 0
}

func containsBytes(hay, needle []byte) bool {
	return strings.Contains(string(hay), string(needle))
}
