package sam3

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// RawSession defines a RAW session on the SAM bridge.
type RawSession struct {
	ID       string       // Session name
	Keys     I2PKeys      // I2P keys
	Conn     net.Conn     // Connection to the SAM control socket
	UDPConn  *net.UDPConn // Used to deliver datagrams
	RUDPAddr *net.UDPAddr // The SAM control socket UDP address
	FromPort string       // FROM_PORT specified on creation
	ToPort   string       // TO_PORT specified on creation
	Protocol string       // PROTOCOL specified on creation
}

// NewRawSession creates a new RawSession on the SAM bridge and returns it.
func (s *SAM) NewRawSession(id string, keys I2PKeys, SAMOpt []string, I2CPOpt []string) (*RawSession, error) {
	// Set defaults first
	udpPort := "7655" // Default SAM UDP port (FROM_PORT/LISTEN_PORT)
	sendPort := "0"   // Default send port (TO_PORT/PORT)
	protocol := "18"  // Default protocol for raw sessions
	lHost, _, err := net.SplitHostPort(s.Conn.LocalAddr().String())
	if err != nil {
		return nil, err
	}

	rHost, _, err := net.SplitHostPort(s.Conn.RemoteAddr().String())
	if err != nil {
		return nil, err
	}

	// Check user options
	for _, opt := range SAMOpt {
		flag := strings.Split(opt, "=")[0]

		if flag == "PORT" || flag == "TO_PORT" {
			sendPort = strings.Split(opt, "=")[1]
			n, err := strconv.Atoi(sendPort)
			if err != nil {
				return nil, err
			}

			if n > 65535 || n < 0 {
				return nil, fmt.Errorf("invalid port %d specified, should be between 0-65535", n)
			}
		}

		if flag == "FROM_PORT" || flag == "LISTEN_PORT" {
			udpPort = strings.Split(opt, "=")[1]
			n, err := strconv.Atoi(udpPort)
			if err != nil {
				return nil, err
			}

			if n > 65535 || n < 0 {
				return nil, fmt.Errorf("invalid port %d specified, should be between 0-65535", n)
			}
		}

		// If passed, verify protocol.
		if flag == "PROTOCOL" || flag == "LISTEN_PROTOCOL" {
			protocol = strings.Split(opt, "=")[1]
			pInt, err := strconv.Atoi(protocol)
			if err != nil {
				return nil, err
			}

			// Check if it's within bounds, and make sure it's not specified as streaming protocol.
			if pInt < 0 || pInt > 255 || pInt == 6 {
				return nil, fmt.Errorf("Bad RAW LISTEN_PROTOCOL %d", pInt)
			}
		}
	}

	// Set up connections to populate session struct with
	lUDPAddr, err := net.ResolveUDPAddr("udp4", lHost+":"+sendPort)
	if err != nil {
		return nil, err
	}

	udpConn, err := net.ListenUDP("udp4", lUDPAddr)
	if err != nil {
		return nil, err
	}

	rUDPAddr, err := net.ResolveUDPAddr("udp4", rHost+":"+udpPort)
	if err != nil {
		return nil, err
	}

	// Format I2CP options
	var sOpt string
	for _, opt := range I2CPOpt {
		sOpt += " OPTION=" + opt
	}

	// Write SESSION CREATE message
	msg := []byte("SESSION CREATE STYLE=RAW ID=" + id + " DESTINATION=" + keys.Priv + " " +
		strings.Join(SAMOpt, " ") + sOpt + "\n")
	text, err := SendToBridge(msg, s.Conn)
	if err != nil {
		return nil, err
	}

	// Check for any returned errors
	if err := s.HandleResponse(text); err != nil {
		return nil, err
	}

	sess := RawSession{
		ID:       id,
		Keys:     keys,
		Conn:     s.Conn,
		UDPConn:  udpConn,
		RUDPAddr: rUDPAddr,
		FromPort: udpPort,
		ToPort:   sendPort,
		Protocol: protocol,
	}

	// Add session to SAM
	s.Session = &sess
	return &sess, nil
}

// Read reads one raw datagram sent to the destination of the DatagramSession.
func (s *RawSession) Read() ([]byte, string, string, error) {
	buf := make([]byte, 32768+67) // Max datagram size + max SAM bridge message size
	n, sAddr, err := s.UDPConn.ReadFromUDP(buf)
	if err != nil {
		return nil, "", "", err
	}

	// Only accept incoming UDP messages from the SAM socket we're connected to.
	if !bytes.Equal(sAddr.IP, s.RUDPAddr.IP) {
		return nil, "", "", fmt.Errorf("datagram received from wrong address: expected %v, actual %v",
			s.RUDPAddr.IP, sAddr.IP)
	}

	// Split message lines first
	i := bytes.IndexByte(buf, byte('\n'))
	msg, data := string(buf[:i]), buf[i+1:n]

	// Split message into fields
	fields := strings.Split(msg, " ")

	// Handle message
	fromPort := "0" // Default FROM_PORT
	toPort := "0"   // Default TO_PORT
	for _, field := range fields {
		switch {
		case strings.Contains(field, "FROM_PORT="):
			fromPort = strings.TrimPrefix(field, "FROM_PORT=")
		case strings.Contains(field, "TO_PORT="):
			toPort = strings.TrimPrefix(field, "TO_PORT=")
		default:
			continue // SIZE is not important as we could determine this from ReadFromUDP
		}
	}

	return data, fromPort, toPort, nil
}

// Write sends one raw datagram to the destination specified. At the time of writing,
// maximum size is 32 kilobyte, but this may change in the future.
func (s *RawSession) Write(b []byte, addr string) (n int, err error) {
	header := []byte("3.3 " + s.ID + " " + addr + " FROM_PORT=" + s.FromPort + " TO_PORT=" + s.ToPort +
		" PROTOCOL=" + s.Protocol + "\n")
	msg := append(header, b...)
	n, err = s.UDPConn.WriteToUDP(msg, s.RUDPAddr)

	return n, err
}

// Close the RawSession.
func (s *RawSession) Close() error {
	WriteMessage([]byte("EXIT"), s.Conn)
	if err := s.Conn.Close(); err != nil {
		return err
	}

	err := s.UDPConn.Close()
	return err
}
