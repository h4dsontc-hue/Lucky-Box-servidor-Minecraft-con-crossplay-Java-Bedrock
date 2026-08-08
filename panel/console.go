package main

import (
	"errors"
	"regexp"
	"sync"
	"time"
)

// Un arranque completo de Paper con estos 6 plugins escupe unas 900 lineas,
// asi que 3000 cubren el arranque entero mas un buen rato de juego.
const consoleCapacity = 3000

type Kind string

const (
	KindOut   Kind = "out"   // stdout de la JVM
	KindErr   Kind = "err"   // stderr de la JVM
	KindPanel Kind = "panel" // mensajes del propio panel
)

type Line struct {
	Seq  int64  `json:"seq"`
	At   string `json:"at"`
	Kind Kind   `json:"kind"`
	Text string `json:"text"`
}

// capture intercepta las lineas que casan con re. Query lo usa para leer la
// respuesta de un comando (por ejemplo "tps") sin tener que adivinar cuando
// llega, y con hide para que las sondas periodicas no ensucien la consola.
type capture struct {
	re   *regexp.Regexp
	hide bool
	out  chan string
}

// Console es un anillo de lineas con suscriptores. Lo alimenta el supervisor
// desde los pipes de la JVM y lo leen los clientes SSE.
type Console struct {
	mu       sync.Mutex
	ring     []Line
	total    int64 // lineas acumuladas; coincide con el Seq de la ultima
	subs     map[int64]chan Line
	nextSub  int64
	captures []*capture
	hooks    []func(Line)
}

func NewConsole() *Console {
	return &Console{
		ring: make([]Line, consoleCapacity),
		subs: make(map[int64]chan Line),
	}
}

// OnLine registra un observador que se ejecuta con cada linea, ya fuera del
// lock. El supervisor lo usa para detectar el arranque y los jugadores.
func (c *Console) OnLine(fn func(Line)) {
	c.mu.Lock()
	c.hooks = append(c.hooks, fn)
	c.mu.Unlock()
}

func (c *Console) Add(kind Kind, text string) {
	c.mu.Lock()
	hide := false
	for _, cp := range c.captures {
		if cp.re.MatchString(text) {
			select {
			case cp.out <- text:
			default: // el lector ya tiene bastante
			}
			if cp.hide {
				hide = true
			}
		}
	}
	if hide {
		c.mu.Unlock()
		return
	}
	c.total++
	ln := Line{Seq: c.total, At: time.Now().Format("15:04:05"), Kind: kind, Text: text}
	c.ring[(c.total-1)%consoleCapacity] = ln

	subs := make([]chan Line, 0, len(c.subs))
	for _, ch := range c.subs {
		subs = append(subs, ch)
	}
	hooks := c.hooks
	c.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ln:
		default:
			// Cliente lento: pierde la linea. El navegador ve el hueco en Seq
			// y la recupera con GET /api/console?since=
		}
	}
	for _, h := range hooks {
		h(ln)
	}
}

// Snapshot devuelve las lineas con Seq > since que siguen en el anillo.
func (c *Console) Snapshot(since int64) []Line {
	c.mu.Lock()
	defer c.mu.Unlock()
	if since >= c.total {
		return []Line{}
	}
	first := c.total - consoleCapacity + 1
	if first < 1 {
		first = 1
	}
	if since+1 > first {
		first = since + 1
	}
	out := make([]Line, 0, c.total-first+1)
	for s := first; s <= c.total; s++ {
		out = append(out, c.ring[(s-1)%consoleCapacity])
	}
	return out
}

func (c *Console) Latest() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func (c *Console) Subscribe() (int64, <-chan Line) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextSub++
	id := c.nextSub
	ch := make(chan Line, 512)
	c.subs[id] = ch
	return id, ch
}

// Unsubscribe no cierra el canal a proposito: Add manda las lineas fuera del
// lock, asi que cerrarlo aqui podria provocar un envio sobre canal cerrado. Sin
// referencias, el recolector se lo lleva.
func (c *Console) Unsubscribe(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.subs, id)
}

func (c *Console) addCapture(cp *capture) {
	c.mu.Lock()
	c.captures = append(c.captures, cp)
	c.mu.Unlock()
}

func (c *Console) removeCapture(cp *capture) {
	c.mu.Lock()
	for i, x := range c.captures {
		if x == cp {
			c.captures = append(c.captures[:i], c.captures[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
}

// Query manda un comando y devuelve la primera linea de respuesta que case con
// re. Con hide=true la respuesta no se muestra en la consola, que es lo que
// queremos para el sondeo automatico de TPS cada pocos segundos.
func (c *Console) Query(send func(string) error, cmd string, re *regexp.Regexp, hide bool, timeout time.Duration) (string, error) {
	cp := &capture{re: re, hide: hide, out: make(chan string, 4)}
	c.addCapture(cp)
	defer c.removeCapture(cp)

	if err := send(cmd); err != nil {
		return "", err
	}
	select {
	case l := <-cp.out:
		return l, nil
	case <-time.After(timeout):
		return "", errors.New("el servidor no respondio a '" + cmd + "'")
	}
}
