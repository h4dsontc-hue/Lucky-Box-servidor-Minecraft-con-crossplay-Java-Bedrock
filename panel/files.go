package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Editor de configuracion. Todo lo que sale de aqui esta restringido al
// directorio del servidor y a extensiones de texto: el panel no es un
// explorador de ficheros de la maquina.

const maxEditableSize = 2 << 20 // 2 MB

var editableExt = map[string]bool{
	".properties": true,
	".yml":        true,
	".yaml":       true,
	".json":       true,
	".txt":        true,
	".conf":       true,
	".cfg":        true,
	".toml":       true,
}

// Directorios que no se listan: o son enormes (el mundo, las librerias de
// Paper) o no contienen nada que se edite a mano.
var skipDirs = map[string]bool{
	"logs":          true,
	"cache":         true,
	"libraries":     true,
	"versions":      true,
	"crash-reports": true,
	".paper":        true,
	"tmp":           true,
	"schematics":    true,
}

// key.pem de Floodgate es un secreto compartido: quien lo tenga puede
// suplantar jugadores de Bedrock. No se lista ni se sirve, punto.
var deniedNames = map[string]bool{
	"key.pem":      true,
	"session.lock": true,
}

type FileEntry struct {
	Path  string `json:"path"` // relativa al directorio del servidor
	Size  int64  `json:"size"`
	Human string `json:"human"`
	Group string `json:"group"`
}

type Files struct {
	dir string
}

func NewFiles(dir string) *Files { return &Files{dir: dir} }

// resolve traduce una ruta relativa del navegador a una ruta absoluta dentro
// del servidor, o falla. filepath.Clean sobre "/"+rel neutraliza los ".."
// antes de unir, y despues se comprueba que el resultado real siga dentro.
func (f *Files) resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("ruta vacia")
	}
	clean := filepath.Clean("/" + filepath.ToSlash(rel))
	abs := filepath.Join(f.dir, clean)

	root, err := filepath.EvalSymlinks(f.dir)
	if err != nil {
		return "", err
	}
	// El fichero puede no existir aun (guardado nuevo): resolvemos su padre.
	check := abs
	if _, err := os.Stat(abs); err != nil {
		check = filepath.Dir(abs)
	}
	real, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", fmt.Errorf("ruta inaccesible: %s", rel)
	}
	if real != root && !strings.HasPrefix(real, root+string(os.PathSeparator)) {
		return "", errors.New("ruta fuera del directorio del servidor")
	}
	if deniedNames[filepath.Base(abs)] {
		return "", errors.New("ese fichero no se toca desde el panel")
	}
	if !editableExt[strings.ToLower(filepath.Ext(abs))] {
		return "", errors.New("solo ficheros de texto de configuracion")
	}
	return abs, nil
}

func (f *Files) List() ([]FileEntry, error) {
	var out []FileEntry
	err := filepath.WalkDir(f.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // un directorio ilegible no debe tumbar el listado
		}
		name := d.Name()
		if d.IsDir() {
			if path == f.dir {
				return nil
			}
			if skipDirs[name] || strings.HasPrefix(name, "world") || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if deniedNames[name] || !editableExt[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxEditableSize {
			return nil
		}
		rel, err := filepath.Rel(f.dir, path)
		if err != nil {
			return nil
		}
		out = append(out, FileEntry{
			Path:  filepath.ToSlash(rel),
			Size:  info.Size(),
			Human: fmtBytes(info.Size()),
			Group: groupOf(filepath.ToSlash(rel)),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return groupRank(out[i].Group) < groupRank(out[j].Group)
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

func groupOf(rel string) string {
	switch {
	case !strings.Contains(rel, "/"):
		return "Servidor"
	case strings.HasPrefix(rel, "config/"):
		return "Paper"
	case strings.HasPrefix(rel, "plugins/"):
		parts := strings.Split(rel, "/")
		if len(parts) >= 3 {
			return parts[1]
		}
		return "Plugins"
	}
	return "Otros"
}

func groupRank(g string) int {
	switch g {
	case "Servidor":
		return 0
	case "Paper":
		return 1
	default:
		return 2
	}
}

func (f *Files) Read(rel string) (string, error) {
	abs, err := f.resolve(rel)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if info.Size() > maxEditableSize {
		return "", fmt.Errorf("%s pesa %s, demasiado para el editor", rel, fmtBytes(info.Size()))
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// Write valida, guarda una copia .bak y escribe de forma atomica.
func (f *Files) Write(rel, content string) (string, error) {
	abs, err := f.resolve(rel)
	if err != nil {
		return "", err
	}
	if len(content) > maxEditableSize {
		return "", errors.New("el contenido excede 2 MB")
	}
	if err := validateContent(abs, content); err != nil {
		return "", err
	}

	note := ""
	if old, err := os.ReadFile(abs); err == nil {
		if err := os.WriteFile(abs+".bak", old, 0o644); err == nil {
			note = " (copia previa en " + filepath.Base(abs) + ".bak)"
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(abs), "."+filepath.Base(abs)+".tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), abs); err != nil {
		return "", err
	}
	return "guardado " + rel + note, nil
}

// validateContent hace la comprobacion barata que evita el error tipico de
// cada formato. No es un parser completo de YAML (eso pediria dependencias):
// detecta la tabulacion, que es lo que rompe a Bukkit al arrancar.
func validateContent(abs, content string) error {
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".json":
		if !json.Valid([]byte(content)) {
			return errors.New("JSON invalido: revisa comas y llaves")
		}
	case ".yml", ".yaml":
		for i, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimLeft(line, " ")
			if len(line)-len(trimmed) > 0 && strings.Contains(line[:len(line)-len(trimmed)], "\t") {
				return fmt.Errorf("linea %d: YAML no admite tabulaciones para indentar, usa espacios", i+1)
			}
			if strings.HasPrefix(trimmed, "\t") {
				return fmt.Errorf("linea %d: YAML no admite tabulaciones para indentar, usa espacios", i+1)
			}
		}
	case ".properties":
		for i, line := range strings.Split(content, "\n") {
			t := strings.TrimSpace(line)
			if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "!") {
				continue
			}
			if !strings.Contains(t, "=") && !strings.Contains(t, ":") {
				return fmt.Errorf("linea %d: falta el '=' (%q)", i+1, t)
			}
		}
	}
	return nil
}

// readProperties lee un .properties de Java a un mapa. Solo se usa para
// consultar ajustes concretos, no para reescribir el fichero entero.
func readProperties(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		// Java escapa ':' como '\:' en level-type=minecraft\:normal
		out[strings.TrimSpace(k)] = strings.ReplaceAll(strings.TrimSpace(v), "\\:", ":")
	}
	return out, nil
}

// setProperty cambia una clave conservando el resto del fichero tal cual:
// comentarios, orden y claves que no entendemos.
func setProperty(path, key, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	found := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, _, ok := strings.Cut(t, "=")
		if ok && strings.TrimSpace(k) == key {
			lines[i] = key + "=" + value
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}
