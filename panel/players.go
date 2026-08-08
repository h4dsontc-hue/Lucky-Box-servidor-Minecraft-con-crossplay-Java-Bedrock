package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Los ficheros de jugadores son los mismos que lee Paper al arrancar. Con el
// servidor arriba NO los tocamos: se manda el comando de consola y que Paper
// los reescriba, porque tiene la lista en memoria y pisaria nuestros cambios
// al apagarse. Con el servidor parado se editan aqui directamente.

type OpEntry struct {
	UUID                string `json:"uuid"`
	Name                string `json:"name"`
	Level               int    `json:"level"`
	BypassesPlayerLimit bool   `json:"bypassesPlayerLimit"`
}

type WhitelistEntry struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type BanEntry struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Created string `json:"created"`
	Source  string `json:"source"`
	Expires string `json:"expires"`
	Reason  string `json:"reason"`
}

type CacheEntry struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	ExpiresOn string `json:"expiresOn"`
}

type KnownPlayer struct {
	Name    string `json:"name"`
	UUID    string `json:"uuid"`
	Bedrock bool   `json:"bedrock"`
}

type PlayersView struct {
	Online       []Player         `json:"online"`
	Ops          []OpEntry        `json:"ops"`
	Whitelist    []WhitelistEntry `json:"whitelist"`
	Bans         []BanEntry       `json:"bans"`
	Known        []KnownPlayer    `json:"known"`
	WhitelistOn  bool             `json:"whitelistOn"`
	ServerUp     bool             `json:"serverUp"`
	OpLevel      int              `json:"opLevel"`
	CanEditFiles bool             `json:"canEditFiles"`
}

type Players struct {
	dir string // directorio del servidor
	sup *Supervisor
}

func NewPlayers(dir string, sup *Supervisor) *Players {
	return &Players{dir: dir, sup: sup}
}

// isBedrockUUID reconoce los UUID que fabrica Floodgate para los jugadores de
// Bedrock: van rellenos de ceros porque derivan del XUID, no de una cuenta
// de Mojang.
func isBedrockUUID(uuid string) bool {
	return strings.HasPrefix(uuid, "00000000-0000-0000-0")
}

func readJSONFile[T any](path string) ([]T, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var out []T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s no es JSON valido: %w", filepath.Base(path), err)
	}
	return out, nil
}

// writeJSONAtomic escribe a un temporal del mismo directorio y renombra. Un
// corte a mitad de escritura dejaria a Paper sin ops.json y sin operadores.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (p *Players) path(name string) string { return filepath.Join(p.dir, name) }

func (p *Players) View() (PlayersView, error) {
	v := PlayersView{
		Ops:       []OpEntry{},
		Whitelist: []WhitelistEntry{},
		Bans:      []BanEntry{},
		Known:     []KnownPlayer{},
		Online:    []Player{},
	}

	ops, err := readJSONFile[OpEntry](p.path("ops.json"))
	if err != nil {
		return v, err
	}
	wl, err := readJSONFile[WhitelistEntry](p.path("whitelist.json"))
	if err != nil {
		return v, err
	}
	bans, err := readJSONFile[BanEntry](p.path("banned-players.json"))
	if err != nil {
		return v, err
	}
	cache, err := readJSONFile[CacheEntry](p.path("usercache.json"))
	if err != nil {
		return v, err
	}

	v.Ops = append(v.Ops, ops...)
	v.Whitelist = append(v.Whitelist, wl...)
	v.Bans = append(v.Bans, bans...)

	// "Conocidos" = todo el que haya entrado alguna vez, mas los que aparecen
	// en las listas. Es la base del selector del navegador.
	seen := map[string]KnownPlayer{}
	add := func(name, uuid string) {
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if cur, ok := seen[key]; ok && cur.UUID != "" {
			return
		}
		seen[key] = KnownPlayer{Name: name, UUID: uuid, Bedrock: isBedrockUUID(uuid) || strings.HasPrefix(name, ".")}
	}
	for _, c := range cache {
		add(c.Name, c.UUID)
	}
	for _, o := range ops {
		add(o.Name, o.UUID)
	}
	for _, w := range wl {
		add(w.Name, w.UUID)
	}
	for _, b := range bans {
		add(b.Name, b.UUID)
	}

	st := p.sup.Status()
	v.Online = st.Players
	for _, pl := range st.Players {
		add(pl.Name, "")
	}
	for _, k := range seen {
		v.Known = append(v.Known, k)
	}
	sort.Slice(v.Known, func(i, j int) bool {
		return strings.ToLower(v.Known[i].Name) < strings.ToLower(v.Known[j].Name)
	})

	v.ServerUp = st.State != StateStopped
	v.CanEditFiles = st.State == StateStopped
	if props, err := readProperties(p.path("server.properties")); err == nil {
		v.WhitelistOn = props["white-list"] == "true"
		v.OpLevel = 4
		if lvl, ok := props["op-permission-level"]; ok {
			fmt.Sscanf(lvl, "%d", &v.OpLevel)
		}
	}
	return v, nil
}

// uuidFor busca el UUID de un jugador en los ficheros locales. Sin servidor
// arriba no hay a quien preguntar: con online-mode=false y Floodgate en juego,
// inventarse un UUID crearia una entrada que Paper ignoraria.
func (p *Players) uuidFor(name string) (string, error) {
	lower := strings.ToLower(name)
	cache, _ := readJSONFile[CacheEntry](p.path("usercache.json"))
	for _, c := range cache {
		if strings.ToLower(c.Name) == lower {
			return c.UUID, nil
		}
	}
	ops, _ := readJSONFile[OpEntry](p.path("ops.json"))
	for _, o := range ops {
		if strings.ToLower(o.Name) == lower {
			return o.UUID, nil
		}
	}
	wl, _ := readJSONFile[WhitelistEntry](p.path("whitelist.json"))
	for _, w := range wl {
		if strings.ToLower(w.Name) == lower {
			return w.UUID, nil
		}
	}
	bans, _ := readJSONFile[BanEntry](p.path("banned-players.json"))
	for _, b := range bans {
		if strings.ToLower(b.Name) == lower {
			return b.UUID, nil
		}
	}
	return "", fmt.Errorf("no se quien es %q: nunca ha entrado a este servidor. Arranca el servidor y que entre una vez, o hazlo con el servidor en marcha", name)
}

var validName = func(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for _, r := range name {
		// Java admite [A-Za-z0-9_]; Floodgate añade el prefijo "." a Bedrock.
		ok := r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// Action aplica op/deop/kick/ban/pardon/whitelist. Devuelve el texto que ve el
// usuario en el navegador.
func (p *Players) Action(action, name, reason string) (string, error) {
	name = strings.TrimSpace(name)
	if !validName(name) {
		return "", fmt.Errorf("nombre de jugador invalido: %q", name)
	}
	reason = strings.TrimSpace(strings.ReplaceAll(reason, "\n", " "))

	up := p.sup.Status().State != StateStopped
	if up {
		return p.viaConsole(action, name, reason)
	}
	return p.viaFiles(action, name, reason)
}

func (p *Players) viaConsole(action, name, reason string) (string, error) {
	var cmd string
	switch action {
	case "op":
		cmd = "op " + name
	case "deop":
		cmd = "deop " + name
	case "kick":
		cmd = "kick " + name
		if reason != "" {
			cmd += " " + reason
		}
	case "ban":
		cmd = "ban " + name
		if reason != "" {
			cmd += " " + reason
		}
	case "pardon":
		cmd = "pardon " + name
	case "whitelist-add":
		cmd = "whitelist add " + name
	case "whitelist-remove":
		cmd = "whitelist remove " + name
	default:
		return "", fmt.Errorf("accion desconocida: %q", action)
	}
	if err := p.sup.Send(cmd); err != nil {
		return "", err
	}
	return "enviado a la consola: " + cmd, nil
}

func (p *Players) viaFiles(action, name, reason string) (string, error) {
	switch action {
	case "op":
		uuid, err := p.uuidFor(name)
		if err != nil {
			return "", err
		}
		ops, err := readJSONFile[OpEntry](p.path("ops.json"))
		if err != nil {
			return "", err
		}
		for _, o := range ops {
			if strings.EqualFold(o.Name, name) {
				return name + " ya era operador", nil
			}
		}
		ops = append(ops, OpEntry{UUID: uuid, Name: name, Level: 4})
		if err := writeJSONAtomic(p.path("ops.json"), ops); err != nil {
			return "", err
		}
		return name + " añadido a ops.json (nivel 4)", nil

	case "deop":
		ops, err := readJSONFile[OpEntry](p.path("ops.json"))
		if err != nil {
			return "", err
		}
		out := ops[:0]
		found := false
		for _, o := range ops {
			if strings.EqualFold(o.Name, name) {
				found = true
				continue
			}
			out = append(out, o)
		}
		if !found {
			return name + " no era operador", nil
		}
		if err := writeJSONAtomic(p.path("ops.json"), out); err != nil {
			return "", err
		}
		return name + " quitado de ops.json", nil

	case "whitelist-add":
		uuid, err := p.uuidFor(name)
		if err != nil {
			return "", err
		}
		wl, err := readJSONFile[WhitelistEntry](p.path("whitelist.json"))
		if err != nil {
			return "", err
		}
		for _, w := range wl {
			if strings.EqualFold(w.Name, name) {
				return name + " ya estaba en la whitelist", nil
			}
		}
		wl = append(wl, WhitelistEntry{UUID: uuid, Name: name})
		if err := writeJSONAtomic(p.path("whitelist.json"), wl); err != nil {
			return "", err
		}
		return name + " añadido a whitelist.json", nil

	case "whitelist-remove":
		wl, err := readJSONFile[WhitelistEntry](p.path("whitelist.json"))
		if err != nil {
			return "", err
		}
		out := wl[:0]
		found := false
		for _, w := range wl {
			if strings.EqualFold(w.Name, name) {
				found = true
				continue
			}
			out = append(out, w)
		}
		if !found {
			return name + " no estaba en la whitelist", nil
		}
		if err := writeJSONAtomic(p.path("whitelist.json"), out); err != nil {
			return "", err
		}
		return name + " quitado de whitelist.json", nil

	case "ban":
		uuid, err := p.uuidFor(name)
		if err != nil {
			return "", err
		}
		bans, err := readJSONFile[BanEntry](p.path("banned-players.json"))
		if err != nil {
			return "", err
		}
		for _, b := range bans {
			if strings.EqualFold(b.Name, name) {
				return name + " ya estaba baneado", nil
			}
		}
		if reason == "" {
			reason = "Banned by an operator."
		}
		bans = append(bans, BanEntry{
			UUID: uuid, Name: name,
			// Mismo formato de fecha que escribe el servidor.
			Created: time.Now().Format("2006-01-02 15:04:05 -0700"),
			Source:  "Panel", Expires: "forever", Reason: reason,
		})
		if err := writeJSONAtomic(p.path("banned-players.json"), bans); err != nil {
			return "", err
		}
		return name + " añadido a banned-players.json", nil

	case "pardon":
		bans, err := readJSONFile[BanEntry](p.path("banned-players.json"))
		if err != nil {
			return "", err
		}
		out := bans[:0]
		found := false
		for _, b := range bans {
			if strings.EqualFold(b.Name, name) {
				found = true
				continue
			}
			out = append(out, b)
		}
		if !found {
			return name + " no estaba baneado", nil
		}
		if err := writeJSONAtomic(p.path("banned-players.json"), out); err != nil {
			return "", err
		}
		return "ban de " + name + " retirado", nil

	case "kick":
		return "", fmt.Errorf("no puedo expulsar a %s: el servidor esta parado", name)
	}
	return "", fmt.Errorf("accion desconocida: %q", action)
}

// SetWhitelistEnforced enciende o apaga la whitelist. Con el servidor arriba se
// usa el comando (que ademas expulsa a los que no estan); parado, se toca
// server.properties.
func (p *Players) SetWhitelistEnforced(on bool) (string, error) {
	if p.sup.Status().State != StateStopped {
		cmd := "whitelist off"
		if on {
			cmd = "whitelist on"
		}
		if err := p.sup.Send(cmd); err != nil {
			return "", err
		}
		return "enviado a la consola: " + cmd, nil
	}
	val := "false"
	if on {
		val = "true"
	}
	if err := setProperty(p.path("server.properties"), "white-list", val); err != nil {
		return "", err
	}
	return "white-list=" + val + " en server.properties", nil
}
