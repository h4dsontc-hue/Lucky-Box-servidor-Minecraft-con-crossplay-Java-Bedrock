package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Collector muestrea en segundo plano todo lo que cuesta tiempo o red, y el
// navegador solo lee la ultima foto. Si /api/status hiciera los pings y el
// "tps" en linea, cada refresco tardaria mas de un segundo.

var reTPSLine = regexp.MustCompile(`TPS from last 1m, 5m, 15m:(.*)`)

// Paper formatea el TPS con el locale de la JVM: en una maquina en español
// imprime "20,0, 20,0, 20,0" con coma decimal. El separador entre valores
// lleva espacio (", "), asi que el numero se puede reconocer sin ambiguedad.
var reFloat = regexp.MustCompile(`\d+(?:[.,]\d+)?`)

type Snapshot struct {
	Server      SupervisorStatus `json:"server"`
	Proc        ProcStats        `json:"proc"`
	Sys         SysStats         `json:"sys"`
	Java        JavaStatus       `json:"java"`
	Bedrock     BedrockStatus    `json:"bedrock"`
	TPS         []float64        `json:"tps"`
	TPSAt       string           `json:"tpsAt,omitempty"`
	Backup      BackupState      `json:"backup"`
	ForeignPID  int              `json:"foreignPid"`
	JavaAddr    string           `json:"javaAddr"`
	BedrockAddr string           `json:"bedrockAddr"`
	ConsoleSeq  int64            `json:"consoleSeq"`
	WorldMB     float64          `json:"worldMB"`
}

type Collector struct {
	sup       *Supervisor
	console   *Console
	backups   *Backups
	serverDir string
	backupDir string

	javaAddr    string
	bedrockAddr string

	mu      sync.Mutex
	last    procSample
	proc    ProcStats
	sys     SysStats
	java    JavaStatus
	bedrock BedrockStatus
	tps     []float64
	tpsAt   time.Time
	foreign int
	worldMB float64
}

func NewCollector(sup *Supervisor, console *Console, backups *Backups, serverDir, backupDir string) *Collector {
	c := &Collector{
		sup:       sup,
		console:   console,
		backups:   backups,
		serverDir: serverDir,
		backupDir: backupDir,
	}
	c.javaAddr = "127.0.0.1:" + portFromProperties(filepath.Join(serverDir, "server.properties"), "server-port", "25565")
	c.bedrockAddr = "127.0.0.1:" + geyserPort(filepath.Join(serverDir, "plugins", "Geyser-Spigot", "config.yml"))
	return c
}

func (c *Collector) Run(stop <-chan struct{}) {
	procTick := time.NewTicker(2 * time.Second)
	pingTick := time.NewTicker(5 * time.Second)
	tpsTick := time.NewTicker(10 * time.Second)
	slowTick := time.NewTicker(30 * time.Second)
	defer func() {
		procTick.Stop()
		pingTick.Stop()
		tpsTick.Stop()
		slowTick.Stop()
	}()

	c.sampleProc()
	c.sampleSlow()

	for {
		select {
		case <-stop:
			return
		case <-procTick.C:
			c.sampleProc()
		case <-pingTick.C:
			c.samplePings()
		case <-tpsTick.C:
			c.sampleTPS()
		case <-slowTick.C:
			c.sampleSlow()
		}
	}
}

func (c *Collector) sampleProc() {
	pid := c.sup.PID()
	if pid <= 0 {
		c.mu.Lock()
		c.proc = ProcStats{}
		c.last = procSample{}
		c.mu.Unlock()
		return
	}
	ticks, rssMB, threads, err := readProc(pid)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.proc = ProcStats{}
		return
	}
	stats := ProcStats{RSSMB: rssMB, Threads: threads, Valid: true}
	if !c.last.at.IsZero() && c.last.ticks > 0 && ticks >= c.last.ticks {
		elapsed := now.Sub(c.last.at).Seconds()
		if elapsed > 0 {
			stats.CPUPercent = (ticks - c.last.ticks) / userHZ / elapsed * 100
		}
	}
	c.last = procSample{ticks: ticks, at: now}
	c.proc = stats
}

func (c *Collector) samplePings() {
	st := c.sup.Status()
	c.mu.Lock()
	foreign := c.foreign
	c.mu.Unlock()

	// Con todo parado no hay a quien preguntar, y el ping UDP a un puerto
	// cerrado se come el timeout entero por nada.
	if st.State == StateStopped && foreign == 0 {
		c.mu.Lock()
		c.java = JavaStatus{Error: "servidor parado"}
		c.bedrock = BedrockStatus{Error: "servidor parado"}
		c.mu.Unlock()
		return
	}
	java := PingJava(c.javaAddr, 2*time.Second)
	bedrock := PingBedrock(c.bedrockAddr, 2*time.Second)
	c.mu.Lock()
	c.java, c.bedrock = java, bedrock
	c.mu.Unlock()
}

// sampleTPS pregunta el TPS por la consola y esconde la respuesta: es la unica
// forma de saberlo sin plugins, pero cada 10 segundos llenaria el log.
func (c *Collector) sampleTPS() {
	if !c.sup.IsReady() {
		c.mu.Lock()
		c.tps = nil
		c.mu.Unlock()
		return
	}
	line, err := c.console.Query(c.sup.Send, "tps", reTPSLine, true, 3*time.Second)
	if err != nil {
		return
	}
	vals := parseTPS(line)
	if vals == nil {
		return
	}
	c.mu.Lock()
	c.tps = vals
	c.tpsAt = time.Now()
	c.mu.Unlock()
}

// parseTPS saca los tres valores de la respuesta de "tps". Devuelve nil si la
// linea no es la que esperabamos.
func parseTPS(line string) []float64 {
	m := reTPSLine.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	var vals []float64
	for _, f := range reFloat.FindAllString(m[1], 3) {
		v, err := strconv.ParseFloat(strings.ReplaceAll(f, ",", "."), 64)
		if err == nil {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return nil
	}
	return vals
}

func (c *Collector) sampleSlow() {
	sys := readSys(c.backupDir)
	worldMB := dirSizeMB(filepath.Join(c.serverDir, "world"))
	foreign := ForeignServerPID(c.serverDir, c.sup.PID())
	c.mu.Lock()
	c.sys = sys
	c.sys.ServerSizeMB = worldMB
	c.worldMB = worldMB
	c.foreign = foreign
	c.mu.Unlock()
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := Snapshot{
		Server:      c.sup.Status(),
		Proc:        c.proc,
		Sys:         c.sys,
		Java:        c.java,
		Bedrock:     c.bedrock,
		TPS:         c.tps,
		Backup:      c.backups.State(),
		ForeignPID:  c.foreign,
		JavaAddr:    c.javaAddr,
		BedrockAddr: c.bedrockAddr,
		ConsoleSeq:  c.console.Latest(),
		WorldMB:     c.worldMB,
	}
	if !c.tpsAt.IsZero() {
		snap.TPSAt = c.tpsAt.Format("15:04:05")
	}
	return snap
}

func portFromProperties(path, key, def string) string {
	props, err := readProperties(path)
	if err != nil {
		return def
	}
	if v, ok := props[key]; ok && v != "" {
		return v
	}
	return def
}

// geyserPort saca el puerto de Bedrock del config.yml de Geyser sin necesitar
// un parser de YAML: basta con quedarse dentro del bloque "bedrock:".
func geyserPort(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "19132"
	}
	defer f.Close()

	inBedrock := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "bedrock:") {
			inBedrock = true
			continue
		}
		if inBedrock {
			// Una clave sin indentar significa que el bloque ya termino.
			if len(line) > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
				break
			}
			t := strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(t, "port:"); ok {
				if p := strings.TrimSpace(v); p != "" {
					return p
				}
			}
		}
	}
	return "19132"
}
