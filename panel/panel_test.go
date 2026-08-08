package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// El TPS se rompio en la primera version porque Paper lo formatea con el locale
// de la JVM: en una maquina en español la linea llega con coma decimal y el
// parseo leia "20,0" como dos numeros, 20 y 0.
func TestParseTPS(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []float64
	}{
		{
			"locale ingles",
			"[13:00:19 INFO]: TPS from last 1m, 5m, 15m: 20.0, 19.5, 18.42",
			[]float64{20.0, 19.5, 18.42},
		},
		{
			"locale español",
			"[13:01:25 INFO]: TPS from last 1m, 5m, 15m: 20,0, 20,0, 20,0",
			[]float64{20, 20, 20},
		},
		{
			"con asterisco de tope",
			"[13:01:25 INFO]: TPS from last 1m, 5m, 15m: *20.0, *20.0, 19.8",
			[]float64{20, 20, 19.8},
		},
		{
			"formato con nombre de hilo",
			"[16:48:03] [Server thread/INFO]: TPS from last 1m, 5m, 15m: 20.0, 20.0, 20.0",
			[]float64{20, 20, 20},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTPS(tc.line)
			if len(got) != len(tc.want) {
				t.Fatalf("parseTPS(%q) = %v, esperaba %v", tc.line, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("valor %d: %v, esperaba %v", i, got[i], tc.want[i])
				}
			}
		})
	}

	if got := parseTPS("[13:00:19 INFO]: There are 0 of a max of 20 players online:"); got != nil {
		t.Errorf("una linea cualquiera no deberia dar TPS, dio %v", got)
	}
}

// El editor solo debe alcanzar ficheros de texto dentro del directorio del
// servidor, y nunca la clave compartida de Floodgate.
func TestFilesResolve(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "plugins", "floodgate"), 0o755)
	os.WriteFile(filepath.Join(dir, "server.properties"), []byte("a=b\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "plugins", "floodgate", "key.pem"), []byte("secreto"), 0o600)
	os.WriteFile(filepath.Join(dir, "paper.jar"), []byte("PK"), 0o644)

	f := NewFiles(dir)

	if _, err := f.resolve("server.properties"); err != nil {
		t.Errorf("server.properties deberia ser editable: %v", err)
	}
	for _, bad := range []string{
		"plugins/floodgate/key.pem",
		"paper.jar",
		"../../../etc/passwd",
		"/etc/passwd",
		"",
	} {
		if _, err := f.resolve(bad); err == nil {
			t.Errorf("resolve(%q) deberia fallar y no fallo", bad)
		}
	}
}

func TestValidateContent(t *testing.T) {
	if err := validateContent("a.yml", "bedrock:\n\tport: 19132\n"); err == nil {
		t.Error("un YAML con tabulacion deberia rechazarse: rompe a Bukkit al arrancar")
	}
	if err := validateContent("a.yml", "bedrock:\n  port: 19132\n"); err != nil {
		t.Errorf("YAML valido rechazado: %v", err)
	}
	if err := validateContent("a.json", "[{,}]"); err == nil {
		t.Error("un JSON roto deberia rechazarse")
	}
	if err := validateContent("a.properties", "esto-no-tiene-igual"); err == nil {
		t.Error("una linea sin '=' deberia rechazarse")
	}
}

// setProperty tiene que cambiar solo la clave pedida: server.properties lleva
// comentarios y valores escapados que no debemos tocar.
func TestSetProperty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.properties")
	original := "#comentario\nwhite-list=false\nlevel-type=minecraft\\:normal\nmotd=hola\n"
	os.WriteFile(path, []byte(original), 0o644)

	if err := setProperty(path, "white-list", "true"); err != nil {
		t.Fatal(err)
	}
	props, err := readProperties(path)
	if err != nil {
		t.Fatal(err)
	}
	if props["white-list"] != "true" {
		t.Errorf("white-list = %q, esperaba true", props["white-list"])
	}
	if props["motd"] != "hola" {
		t.Errorf("motd se ha estropeado: %q", props["motd"])
	}
	if props["level-type"] != "minecraft:normal" {
		t.Errorf("level-type = %q, esperaba minecraft:normal", props["level-type"])
	}
	raw, _ := os.ReadFile(path)
	if want := "#comentario"; string(raw[:len(want)]) != want {
		t.Error("el comentario de cabecera se ha perdido")
	}

	// Una clave que no existe se añade al final.
	if err := setProperty(path, "clave-nueva", "1"); err != nil {
		t.Fatal(err)
	}
	props, _ = readProperties(path)
	if props["clave-nueva"] != "1" {
		t.Error("no se añadio la clave nueva")
	}
}

// El anillo de consola tiene que entregar los huecos por Seq: es lo que usa el
// navegador para recuperarse cuando pierde lineas.
func TestConsoleRing(t *testing.T) {
	c := NewConsole()
	for i := 0; i < consoleCapacity+50; i++ {
		c.Add(KindOut, "linea")
	}
	if got := c.Latest(); got != int64(consoleCapacity+50) {
		t.Errorf("Latest = %d, esperaba %d", got, consoleCapacity+50)
	}
	all := c.Snapshot(0)
	if len(all) != consoleCapacity {
		t.Errorf("Snapshot(0) devolvio %d lineas, el anillo guarda %d", len(all), consoleCapacity)
	}
	if all[0].Seq != 51 {
		t.Errorf("la linea mas antigua es la %d, esperaba la 51", all[0].Seq)
	}
	if got := c.Snapshot(c.Latest()); len(got) != 0 {
		t.Errorf("Snapshot al dia deberia venir vacio, trajo %d", len(got))
	}
	if got := c.Snapshot(c.Latest() - 3); len(got) != 3 {
		t.Errorf("esperaba 3 lineas de retraso, trajo %d", len(got))
	}
}

func TestPlayersFileActions(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "start.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "usercache.json"),
		[]byte(`[{"uuid":"669a200f-7ecf-3822-8e9d-b8f8a1129ab5","name":"Papa"}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "ops.json"), []byte("[]"), 0o644)

	sup, err := NewSupervisor(dir, "1G", "1G", NewConsole())
	if err != nil {
		t.Fatal(err)
	}
	p := NewPlayers(dir, sup)

	if _, err := p.Action("op", "Papa", ""); err != nil {
		t.Fatalf("op con el servidor parado deberia escribir ops.json: %v", err)
	}
	ops, err := readJSONFile[OpEntry](filepath.Join(dir, "ops.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Name != "Papa" || ops[0].Level != 4 {
		t.Fatalf("ops.json quedo como %+v", ops)
	}
	// Paper tiene que poder leerlo: mismo formato de siempre.
	raw, _ := os.ReadFile(filepath.Join(dir, "ops.json"))
	var check []map[string]any
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("ops.json no es JSON valido: %v", err)
	}

	// Sin UUID conocido no se puede inventar la entrada.
	if _, err := p.Action("op", "Desconocido", ""); err == nil {
		t.Error("op a un jugador que nunca entro deberia fallar con el servidor parado")
	}
	if _, err := p.Action("kick", "Papa", ""); err == nil {
		t.Error("kick con el servidor parado deberia fallar")
	}
	if _, err := p.Action("deop", "Papa", ""); err != nil {
		t.Fatal(err)
	}
	ops, _ = readJSONFile[OpEntry](filepath.Join(dir, "ops.json"))
	if len(ops) != 0 {
		t.Errorf("deop no limpio ops.json: %+v", ops)
	}
}

func TestGeyserPort(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	os.WriteFile(cfg, []byte("bedrock:\n  address: 0.0.0.0\n  port: 19133\n  clone-remote-port: false\nremote:\n  port: 25565\n"), 0o644)
	if got := geyserPort(cfg); got != "19133" {
		t.Errorf("geyserPort = %q, esperaba 19133 (el puerto de bedrock, no el de remote)", got)
	}
	if got := geyserPort(filepath.Join(dir, "no-existe.yml")); got != "19132" {
		t.Errorf("sin fichero deberia caer al puerto por defecto, dio %q", got)
	}
}

func TestBackupNamePattern(t *testing.T) {
	dir := t.TempDir()
	bk, err := NewBackups(dir, dir, nil, NewConsole())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"world.tar.gz",
		"",
	} {
		if _, err := bk.Path(bad); err == nil {
			t.Errorf("Path(%q) deberia rechazarse", bad)
		}
	}
	good := "world-2026-08-08_130209.tar.gz"
	os.WriteFile(filepath.Join(dir, good), []byte("x"), 0o644)
	if _, err := bk.Path(good); err != nil {
		t.Errorf("Path(%q) deberia valer: %v", good, err)
	}
}
