package main

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// USER_HZ en Linux es 100 en todas las arquitecturas que nos importan; es el
// valor con el que el kernel expone utime/stime en /proc/<pid>/stat.
const userHZ = 100.0

type ProcStats struct {
	CPUPercent float64 `json:"cpuPercent"` // sobre un nucleo: 200 = dos nucleos saturados
	RSSMB      float64 `json:"rssMB"`
	Threads    int     `json:"threads"`
	Valid      bool    `json:"valid"`
}

type SysStats struct {
	CPUs         int     `json:"cpus"`
	Load1        float64 `json:"load1"`
	MemTotalMB   float64 `json:"memTotalMB"`
	MemFreeMB    float64 `json:"memFreeMB"`
	DiskFreeGB   float64 `json:"diskFreeGB"`
	DiskTotalGB  float64 `json:"diskTotalGB"`
	ServerSizeMB float64 `json:"serverSizeMB"`
}

type procSample struct {
	ticks float64
	at    time.Time
}

// readProc saca de /proc lo que un panel necesita del proceso de la JVM.
func readProc(pid int) (float64, float64, int, error) {
	if pid <= 0 {
		return 0, 0, 0, errors.New("sin pid")
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, 0, err
	}
	// El campo comm va entre parentesis y puede contener espacios, asi que hay
	// que cortar por el ULTIMO ')' antes de trocear por espacios.
	s := string(raw)
	close := strings.LastIndexByte(s, ')')
	if close < 0 || close+2 >= len(s) {
		return 0, 0, 0, errors.New("/proc/stat con formato inesperado")
	}
	fields := strings.Fields(s[close+2:])
	// Tras comm y state, fields[0] es ppid (campo 4 del formato de proc(5)).
	// utime es el 14 -> fields[11]; stime el 15 -> fields[12];
	// num_threads el 20 -> fields[17].
	if len(fields) < 18 {
		return 0, 0, 0, errors.New("/proc/stat incompleto")
	}
	utime, _ := strconv.ParseFloat(fields[11], 64)
	stime, _ := strconv.ParseFloat(fields[12], 64)
	threads, _ := strconv.Atoi(fields[17])

	var rssMB float64
	if statm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm"); err == nil {
		f := strings.Fields(string(statm))
		if len(f) >= 2 {
			pages, _ := strconv.ParseFloat(f[1], 64)
			rssMB = pages * float64(os.Getpagesize()) / (1024 * 1024)
		}
	}
	return utime + stime, rssMB, threads, nil
}

func readSys(paths ...string) SysStats {
	st := SysStats{CPUs: runtime.NumCPU()}

	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 0 {
			st.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			key, val, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			kb, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(val), " kB"), 64)
			switch key {
			case "MemTotal":
				st.MemTotalMB = kb / 1024
			case "MemAvailable":
				st.MemFreeMB = kb / 1024
			}
		}
	}
	if len(paths) > 0 {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(paths[0], &fs); err == nil {
			st.DiskFreeGB = float64(fs.Bavail) * float64(fs.Bsize) / (1 << 30)
			st.DiskTotalGB = float64(fs.Blocks) * float64(fs.Bsize) / (1 << 30)
		}
	}
	return st
}

func dirSizeMB(root string) float64 {
	var total int64
	// Sin filepath.WalkDir sobre enlaces: el mundo puede tener muchos ficheros
	// pero ninguno enlazado, y asi no nos vamos por una rama ajena.
	entries := []string{root}
	for len(entries) > 0 {
		dir := entries[len(entries)-1]
		entries = entries[:len(entries)-1]
		list, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range list {
			p := dir + "/" + e.Name()
			if e.IsDir() {
				entries = append(entries, p)
				continue
			}
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	return float64(total) / (1024 * 1024)
}
