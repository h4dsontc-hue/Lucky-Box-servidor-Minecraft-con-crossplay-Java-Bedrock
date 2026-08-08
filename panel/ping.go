package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Este fichero habla los dos protocolos de "ver el servidor desde fuera":
//
//   - Java: Server List Ping sobre TCP 25565, el mismo que usa el cliente al
//     mostrar el servidor en la lista.
//   - Bedrock: ping no conectado de RakNet sobre UDP 19132, que es el que
//     responde Geyser.
//
// Preguntar por ahi es la unica comprobacion honesta de que el crossplay
// funciona: que el proceso este vivo no garantiza que Geyser haya abierto UDP.

type JavaStatus struct {
	OK        bool     `json:"ok"`
	Error     string   `json:"error,omitempty"`
	Version   string   `json:"version,omitempty"`
	Protocol  int      `json:"protocol,omitempty"`
	Online    int      `json:"online"`
	Max       int      `json:"max"`
	MOTD      string   `json:"motd,omitempty"`
	Sample    []string `json:"sample,omitempty"`
	LatencyMS int64    `json:"latencyMs"`
}

type BedrockStatus struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Edition   string `json:"edition,omitempty"`
	MOTD      string `json:"motd,omitempty"`
	Version   string `json:"version,omitempty"`
	Online    int    `json:"online"`
	Max       int    `json:"max"`
	LatencyMS int64  `json:"latencyMs"`
}

func writeVarInt(b *strings.Builder, v int32) {
	uv := uint32(v)
	for {
		if uv&^0x7F == 0 {
			b.WriteByte(byte(uv))
			return
		}
		b.WriteByte(byte(uv&0x7F) | 0x80)
		uv >>= 7
	}
}

func writeString(b *strings.Builder, s string) {
	writeVarInt(b, int32(len(s)))
	b.WriteString(s)
}

func readVarInt(r io.Reader) (int32, error) {
	var result uint32
	for i := 0; i < 5; i++ {
		var buf [1]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, err
		}
		result |= uint32(buf[0]&0x7F) << (7 * i)
		if buf[0]&0x80 == 0 {
			return int32(result), nil
		}
	}
	return 0, errors.New("varint demasiado largo")
}

// PingJava hace el handshake de estado y devuelve el JSON que el servidor
// publica en la lista de servidores.
func PingJava(addr string, timeout time.Duration) JavaStatus {
	start := time.Now()
	st := JavaStatus{}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		st.Error = "direccion invalida: " + addr
		return st
	}
	port, _ := strconv.Atoi(portStr)

	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		st.Error = "sin respuesta en " + addr
		return st
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	// Handshake: protocolo -1 significa "no me importa la version", el
	// servidor contesta el estado igual.
	var payload strings.Builder
	writeVarInt(&payload, 0x00)
	writeVarInt(&payload, -1)
	writeString(&payload, host)
	binary.Write(&hostPortWriter{&payload}, binary.BigEndian, uint16(port))
	writeVarInt(&payload, 1) // siguiente estado: status

	var packet strings.Builder
	writeVarInt(&packet, int32(payload.Len()))
	packet.WriteString(payload.String())
	// Peticion de estado: paquete vacio con id 0x00.
	writeVarInt(&packet, 1)
	writeVarInt(&packet, 0x00)

	if _, err := io.WriteString(conn, packet.String()); err != nil {
		st.Error = err.Error()
		return st
	}

	length, err := readVarInt(conn)
	if err != nil || length <= 0 || length > 1<<20 {
		st.Error = "respuesta ilegible del servidor Java"
		return st
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		st.Error = "respuesta truncada"
		return st
	}
	rest := body
	if _, n := parseVarInt(rest); n > 0 { // id de paquete
		rest = rest[n:]
	}
	strLen, n := parseVarInt(rest)
	if n <= 0 || int(strLen) > len(rest)-n {
		st.Error = "JSON de estado invalido"
		return st
	}
	raw := rest[n : n+int(strLen)]

	var parsed struct {
		Version struct {
			Name     string `json:"name"`
			Protocol int    `json:"protocol"`
		} `json:"version"`
		Players struct {
			Max    int `json:"max"`
			Online int `json:"online"`
			Sample []struct {
				Name string `json:"name"`
			} `json:"sample"`
		} `json:"players"`
		Description json.RawMessage `json:"description"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		st.Error = "JSON de estado invalido: " + err.Error()
		return st
	}

	st.OK = true
	st.Version = parsed.Version.Name
	st.Protocol = parsed.Version.Protocol
	st.Online = parsed.Players.Online
	st.Max = parsed.Players.Max
	st.MOTD = flattenChat(parsed.Description)
	for _, p := range parsed.Players.Sample {
		st.Sample = append(st.Sample, p.Name)
	}
	st.LatencyMS = time.Since(start).Milliseconds()
	return st
}

type hostPortWriter struct{ b *strings.Builder }

func (w *hostPortWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

func parseVarInt(b []byte) (int32, int) {
	var result uint32
	for i := 0; i < len(b) && i < 5; i++ {
		result |= uint32(b[i]&0x7F) << (7 * i)
		if b[i]&0x80 == 0 {
			return int32(result), i + 1
		}
	}
	return 0, 0
}

// flattenChat aplasta un componente de chat a texto plano. El MOTD puede venir
// como cadena suelta o como arbol {text, extra:[...]}, y ademas trae codigos §.
func flattenChat(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return reSection.ReplaceAllString(s, "")
	}
	var node struct {
		Text  string            `json:"text"`
		Extra []json.RawMessage `json:"extra"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(node.Text)
	for _, e := range node.Extra {
		b.WriteString(flattenChat(e))
	}
	return reSection.ReplaceAllString(b.String(), "")
}

// magico de RakNet: constante fija del protocolo, identifica el paquete como
// un ping no conectado.
var rakNetMagic = []byte{
	0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe,
	0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78,
}

// PingBedrock manda un Unconnected Ping a Geyser. Es UDP: si el puerto 19132
// esta cerrado en el firewall no hay error de conexion, solo silencio, y por
// eso hay timeout.
func PingBedrock(addr string, timeout time.Duration) BedrockStatus {
	start := time.Now()
	st := BedrockStatus{}

	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		st.Error = "no pude abrir UDP hacia " + addr
		return st
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	req := make([]byte, 0, 33)
	req = append(req, 0x01) // ID_UNCONNECTED_PING
	req = binary.BigEndian.AppendUint64(req, uint64(time.Now().UnixMilli()))
	req = append(req, rakNetMagic...)
	guid := make([]byte, 8)
	rand.Read(guid)
	req = append(req, guid...)

	if _, err := conn.Write(req); err != nil {
		st.Error = err.Error()
		return st
	}

	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		st.Error = "sin respuesta en " + addr + " (¿Geyser arrancado? ¿19132/udp abierto?)"
		return st
	}
	// 0x1c + timestamp(8) + serverGUID(8) + magic(16) + len(2) + cadena
	if n < 35 || buf[0] != 0x1c {
		st.Error = "respuesta RakNet inesperada"
		return st
	}
	strLen := int(binary.BigEndian.Uint16(buf[33:35]))
	if 35+strLen > n {
		strLen = n - 35
	}
	fields := strings.Split(string(buf[35:35+strLen]), ";")
	st.OK = true
	st.LatencyMS = time.Since(start).Milliseconds()
	// MCPE;MOTD;protocolo;version;jugadores;max;guid;submotd;...
	get := func(i int) string {
		if i < len(fields) {
			return fields[i]
		}
		return ""
	}
	st.Edition = get(0)
	st.MOTD = reSection.ReplaceAllString(get(1), "")
	st.Version = get(3)
	st.Online, _ = strconv.Atoi(get(4))
	st.Max, _ = strconv.Atoi(get(5))
	return st
}

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	val := float64(n)
	for _, u := range units {
		val /= unit
		if val < unit {
			return fmt.Sprintf("%.1f %s", val, u)
		}
	}
	return fmt.Sprintf("%.1f PB", val/unit)
}
