package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Backup del mundo en tar.gz.
//
// Lo importante no es el tar, es el orden: con el servidor arriba Paper tiene
// chunks sin volcar en memoria, asi que primero "save-off" para que deje de
// escribir, luego "save-all flush" para que baje todo a disco, y solo entonces
// se copia. Al final "save-on" SIEMPRE, incluso si el tar falla: dejar el
// servidor sin autoguardado seria mucho peor que quedarse sin backup.

var backupName = regexp.MustCompile(`^world-\d{4}-\d{2}-\d{2}_\d{6}\.tar\.gz$`)

var (
	reSaveDisabled = regexp.MustCompile(`(?i)Automatic saving is now disabled|Turned off world auto-saving`)
	reSaved        = regexp.MustCompile(`(?i)Saved the game`)
)

type BackupInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Human string `json:"human"`
	At    string `json:"at"`
}

type BackupState struct {
	Running bool   `json:"running"`
	Step    string `json:"step,omitempty"`
	Last    string `json:"last,omitempty"`
	LastErr string `json:"lastErr,omitempty"`
}

type Backups struct {
	dir       string // donde se guardan los tar.gz
	serverDir string
	sup       *Supervisor
	console   *Console

	mu    sync.Mutex
	state BackupState
}

func NewBackups(dir, serverDir string, sup *Supervisor, console *Console) (*Backups, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Backups{dir: dir, serverDir: serverDir, sup: sup, console: console}, nil
}

func (b *Backups) State() BackupState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Backups) setStep(step string) {
	b.mu.Lock()
	b.state.Step = step
	b.mu.Unlock()
	if step != "" {
		b.console.Add(KindPanel, "== panel: backup - "+step)
	}
}

// worldDirs devuelve los mundos que existen ahora mismo. El nether y el end
// solo aparecen cuando alguien los visita, asi que se comprueba cada vez.
func (b *Backups) worldDirs() []string {
	var out []string
	for _, name := range []string{"world", "world_nether", "world_the_end"} {
		p := filepath.Join(b.serverDir, name)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			out = append(out, name)
		}
	}
	return out
}

// Start lanza el backup en segundo plano: un mundo grande tarda mas de lo que
// aguanta una peticion HTTP, y el progreso ya se ve en la consola.
func (b *Backups) Start() error {
	b.mu.Lock()
	if b.state.Running {
		b.mu.Unlock()
		return errors.New("ya hay un backup en curso")
	}
	worlds := b.worldDirs()
	if len(worlds) == 0 {
		b.mu.Unlock()
		return errors.New("no encuentro ningun directorio de mundo que copiar")
	}
	b.state = BackupState{Running: true, Step: "preparando"}
	b.mu.Unlock()

	go func() {
		name, err := b.run(worlds)
		b.mu.Lock()
		b.state.Running = false
		b.state.Step = ""
		if err != nil {
			b.state.LastErr = err.Error()
			b.state.Last = ""
		} else {
			b.state.LastErr = ""
			b.state.Last = name
		}
		b.mu.Unlock()
		if err != nil {
			b.console.Add(KindPanel, "== panel: backup FALLIDO: "+err.Error())
		} else {
			b.console.Add(KindPanel, "== panel: backup listo -> "+name)
		}
	}()
	return nil
}

func (b *Backups) run(worlds []string) (string, error) {
	live := b.sup.IsReady()

	if live {
		b.setStep("desactivando el autoguardado")
		if _, err := b.console.Query(b.sup.Send, "save-off", reSaveDisabled, false, 10*time.Second); err != nil {
			// No abortamos: puede que el servidor este cargando todavia. Con
			// save-on al final el estado queda correcto igualmente.
			b.console.Add(KindPanel, "== panel: aviso, save-off sin confirmar ("+err.Error()+")")
		}
		// Pase lo que pase, hay que devolver el autoguardado.
		defer func() {
			if err := b.sup.Send("save-on"); err != nil {
				b.console.Add(KindPanel, "== panel: ATENCION no pude reactivar el autoguardado: "+err.Error())
			} else {
				b.console.Add(KindPanel, "== panel: autoguardado reactivado (save-on)")
			}
		}()

		b.setStep("volcando el mundo a disco (save-all flush)")
		if _, err := b.console.Query(b.sup.Send, "save-all flush", reSaved, false, 120*time.Second); err != nil {
			return "", fmt.Errorf("el servidor no confirmo el guardado: %w", err)
		}
	} else {
		b.console.Add(KindPanel, "== panel: servidor parado, copiando el mundo tal cual esta en disco")
	}

	name := "world-" + time.Now().Format("2006-01-02_150405") + ".tar.gz"
	final := filepath.Join(b.dir, name)
	tmp := final + ".part"

	b.setStep("comprimiendo " + strings.Join(worlds, ", "))
	if err := b.writeArchive(tmp, worlds); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if info, err := os.Stat(final); err == nil {
		return fmt.Sprintf("%s (%s)", name, fmtBytes(info.Size())), nil
	}
	return name, nil
}

func (b *Backups) writeArchive(dest string, worlds []string) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	// Las region files ya vienen comprimidas con deflate, asi que apretar mas
	// cuesta CPU y no gana tamaño: BestSpeed es el ajuste correcto aqui.
	gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)

	for _, world := range worlds {
		base := filepath.Join(b.serverDir, world)
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // un fichero que desaparece a mitad no invalida el backup
			}
			rel, err := filepath.Rel(b.serverDir, path)
			if err != nil {
				return nil
			}
			// Solo directorios y ficheros normales: los sockets o enlaces del
			// directorio del mundo no aportan nada al restaurar.
			if !info.Mode().IsRegular() && !info.IsDir() {
				return nil
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return nil
			}
			hdr.Name = filepath.ToSlash(rel)
			if info.IsDir() {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			src, err := os.Open(path)
			if err != nil {
				return nil
			}
			defer src.Close()
			written, err := io.Copy(tw, src)
			if err != nil {
				return err
			}
			// Si el fichero crecio mientras lo copiabamos, el tar quedaria
			// descuadrado: rellenamos hasta el tamaño declarado.
			if written < hdr.Size {
				if _, err := io.CopyN(tw, zeroReader{}, hdr.Size-written); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			tw.Close()
			gz.Close()
			return err
		}
	}

	if err := tw.Close(); err != nil {
		gz.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Sync()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func (b *Backups) List() ([]BackupInfo, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}
	out := []BackupInfo{}
	for _, e := range entries {
		if e.IsDir() || !backupName.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{
			Name:  e.Name(),
			Size:  info.Size(),
			Human: fmtBytes(info.Size()),
			At:    info.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Path valida el nombre contra el patron de los backups y devuelve su ruta.
// Asi el borrado y la descarga no pueden apuntar a nada mas.
func (b *Backups) Path(name string) (string, error) {
	if filepath.Base(name) != name || !backupName.MatchString(name) {
		return "", errors.New("nombre de backup invalido")
	}
	p := filepath.Join(b.dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", errors.New("ese backup no existe")
	}
	return p, nil
}

func (b *Backups) Delete(name string) error {
	p, err := b.Path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	b.console.Add(KindPanel, "== panel: backup borrado - "+name)
	return nil
}
