package skirk

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

type SOCKSHandler func(ctx context.Context, target string, conn net.Conn)

type SOCKSServer struct {
	Listen  string
	Handler SOCKSHandler
	Logger  *log.Logger
}

type socksRequest struct {
	Command byte
	Target  string
}

func (s *SOCKSServer) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *SOCKSServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	req, err := socksHandshake(conn)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Printf("socks handshake failed: %v", err)
		}
		return
	}
	if req.Command == socksCommandUDPAssociate {
		if err := s.serveUDPAssociate(ctx, conn); err != nil && s.Logger != nil {
			s.Logger.Printf("socks udp associate failed: %v", err)
		}
		return
	}
	if req.Command == socksCommandUDPInTCP {
		if err := s.serveUDPInTCP(ctx, conn); err != nil && s.Logger != nil {
			s.Logger.Printf("socks udp-in-tcp failed: %v", err)
		}
		return
	}
	if isMappedDNSOverTLSProbe(req.Target) {
		// Android may probe Private DNS over TCP/853 against the VPN DNS
		// address. Rejecting it immediately avoids a long target-dial timeout
		// and lets Android fall back to the UDP DNS path handled above.
		_ = socksReply(conn, 0x04)
		return
	}
	if s.Handler == nil {
		_ = socksReply(conn, 0x01)
		return
	}
	if err := socksReply(conn, 0x00); err != nil {
		return
	}
	s.Handler(ctx, req.Target, conn)
}

func socksHandshake(conn net.Conn) (socksRequest, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return socksRequest{}, err
	}
	if head[0] != 0x05 {
		return socksRequest{}, fmt.Errorf("unsupported socks version %d", head[0])
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return socksRequest{}, err
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return socksRequest{}, err
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return socksRequest{}, err
	}
	if req[0] != 0x05 {
		return socksRequest{}, fmt.Errorf("unsupported socks request version %d", req[0])
	}
	if req[1] != socksCommandConnect && req[1] != socksCommandUDPAssociate && req[1] != socksCommandUDPInTCP {
		return socksRequest{}, fmt.Errorf("unsupported socks5 command 0x%02x", req[1])
	}
	var host string
	switch req[3] {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return socksRequest{}, err
		}
		host = net.IP(buf).String()
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return socksRequest{}, err
		}
		buf := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return socksRequest{}, err
		}
		host = string(buf)
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return socksRequest{}, err
		}
		host = net.IP(buf).String()
	default:
		return socksRequest{}, fmt.Errorf("unsupported address type 0x%02x", req[3])
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return socksRequest{}, err
	}
	port := binary.BigEndian.Uint16(portBuf)
	return socksRequest{
		Command: req[1],
		Target:  net.JoinHostPort(host, strconv.Itoa(int(port))),
	}, nil
}

func isMappedDNSOverTLSProbe(target string) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil || port != "853" {
		return false
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return false
	}
	return ip[0] == 198 && (ip[1] == 18 || ip[1] == 19)
}

func socksReply(conn net.Conn, rep byte) error {
	reply := []byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	_, err := conn.Write(reply)
	return err
}

func (s *SOCKSServer) serveUDPInTCP(ctx context.Context, control net.Conn) error {
	if err := socksReply(control, 0x00); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		packet, err := readUDPInTCPFrame(control)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		if packet.Port != 53 {
			return fmt.Errorf("udp-in-tcp non-dns target refused: %s:%d", packet.Host, packet.Port)
		}
		response, err := answerDNSQuery(packet.Payload)
		if err != nil {
			if s.Logger != nil {
				s.Logger.Printf("socks udp-in-tcp dns failed: %v", err)
			}
			continue
		}
		if _, err := control.Write(buildUDPInTCPFrame(packet.Host, packet.Port, response)); err != nil {
			return err
		}
	}
}

func (s *SOCKSServer) serveUDPAssociate(ctx context.Context, control net.Conn) error {
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		_ = socksReply(control, 0x01)
		return err
	}
	defer udpConn.Close()
	addr := udpConn.LocalAddr().(*net.UDPAddr)
	if err := socksReplyUDP(control, 0x00, addr.IP, addr.Port); err != nil {
		return err
	}

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, control)
		close(done)
	}()

	buf := make([]byte, 64*1024)
	for {
		_ = udpConn.SetReadDeadline(time.Now().Add(time.Second))
		n, clientAddr, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-done:
				return nil
			default:
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		}
		packet, err := parseSOCKSUDPDatagram(buf[:n])
		if err != nil {
			if s.Logger != nil {
				s.Logger.Printf("socks udp datagram ignored: %v", err)
			}
			continue
		}
		if packet.Port != 53 {
			return fmt.Errorf("udp non-dns target refused: %s:%d", packet.Host, packet.Port)
		}
		response, err := answerDNSQuery(packet.Payload)
		if err != nil {
			if s.Logger != nil {
				s.Logger.Printf("socks udp dns failed: %v", err)
			}
			continue
		}
		reply := buildSOCKSUDPDatagram(packet.Host, packet.Port, response)
		_, _ = udpConn.WriteToUDP(reply, clientAddr)
	}
}

func socksReplyUDP(conn net.Conn, rep byte, ip net.IP, port int) error {
	ip4 := ip.To4()
	if ip4 == nil {
		ip4 = net.IPv4(127, 0, 0, 1)
	}
	reply := []byte{0x05, rep, 0x00, 0x01, ip4[0], ip4[1], ip4[2], ip4[3], 0, 0}
	binary.BigEndian.PutUint16(reply[8:10], uint16(port))
	_, err := conn.Write(reply)
	return err
}

func readUDPInTCPFrame(r io.Reader) (socksUDPPacket, error) {
	head := make([]byte, 3)
	if _, err := io.ReadFull(r, head); err != nil {
		return socksUDPPacket{}, err
	}
	dataLen := int(binary.BigEndian.Uint16(head[:2]))
	headerLen := int(head[2])
	if headerLen < 7 {
		return socksUDPPacket{}, fmt.Errorf("invalid udp-in-tcp header length %d", headerLen)
	}
	address := make([]byte, headerLen-3)
	if _, err := io.ReadFull(r, address); err != nil {
		return socksUDPPacket{}, err
	}
	payload := make([]byte, dataLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return socksUDPPacket{}, err
	}
	host, port, err := parseSOCKSAddress(address)
	if err != nil {
		return socksUDPPacket{}, err
	}
	return socksUDPPacket{Host: host, Port: port, Payload: payload}, nil
}

func buildUDPInTCPFrame(host string, port int, payload []byte) []byte {
	address := buildSOCKSAddress(host, port)
	out := make([]byte, 3+len(address)+len(payload))
	binary.BigEndian.PutUint16(out[:2], uint16(len(payload)))
	out[2] = byte(3 + len(address))
	copy(out[3:], address)
	copy(out[3+len(address):], payload)
	return out
}

type socksUDPPacket struct {
	Host    string
	Port    int
	Payload []byte
}

func parseSOCKSUDPDatagram(data []byte) (socksUDPPacket, error) {
	if len(data) < 10 || data[0] != 0 || data[1] != 0 || data[2] != 0 {
		return socksUDPPacket{}, fmt.Errorf("invalid socks udp header")
	}
	host, port, offset, err := parseSOCKSAddressWithOffset(data, 4)
	if err != nil {
		return socksUDPPacket{}, err
	}
	return socksUDPPacket{Host: host, Port: port, Payload: data[offset:]}, nil
}

func parseSOCKSAddress(data []byte) (string, int, error) {
	host, port, offset, err := parseSOCKSAddressWithOffset(data, 0)
	if err != nil {
		return "", 0, err
	}
	if offset != len(data) {
		return "", 0, fmt.Errorf("unexpected address padding")
	}
	return host, port, nil
}

func parseSOCKSAddressWithOffset(data []byte, offset int) (string, int, int, error) {
	var host string
	if len(data) <= offset {
		return "", 0, 0, io.ErrUnexpectedEOF
	}
	switch data[offset] {
	case 0x01:
		offset++
		if len(data) < offset+4+2 {
			return "", 0, 0, io.ErrUnexpectedEOF
		}
		host = net.IP(data[offset : offset+4]).String()
		offset += 4
	case 0x03:
		offset++
		if len(data) < offset+1 {
			return "", 0, 0, io.ErrUnexpectedEOF
		}
		nameLen := int(data[offset])
		offset++
		if len(data) < offset+nameLen+2 {
			return "", 0, 0, io.ErrUnexpectedEOF
		}
		host = string(data[offset : offset+nameLen])
		offset += nameLen
	case 0x04:
		offset++
		if len(data) < offset+16+2 {
			return "", 0, 0, io.ErrUnexpectedEOF
		}
		host = net.IP(data[offset : offset+16]).String()
		offset += 16
	default:
		return "", 0, 0, fmt.Errorf("unsupported socks address type 0x%02x", data[offset])
	}
	port := int(binary.BigEndian.Uint16(data[offset : offset+2]))
	offset += 2
	return host, port, offset, nil
}

func buildSOCKSUDPDatagram(host string, port int, payload []byte) []byte {
	address := buildSOCKSAddress(host, port)
	out := make([]byte, 3+len(address)+len(payload))
	copy(out[3:], address)
	copy(out[3+len(address):], payload)
	return out
}

func buildSOCKSAddress(host string, port int) []byte {
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		out := make([]byte, 7)
		out[0] = 0x01
		copy(out[1:5], ip4)
		binary.BigEndian.PutUint16(out[5:7], uint16(port))
		return out
	}
	hostBytes := []byte(host)
	if len(hostBytes) > 255 {
		hostBytes = hostBytes[:255]
	}
	out := make([]byte, 4+len(hostBytes))
	out[0] = 0x03
	out[1] = byte(len(hostBytes))
	copy(out[2:], hostBytes)
	binary.BigEndian.PutUint16(out[2+len(hostBytes):4+len(hostBytes)], uint16(port))
	return out
}

func answerDNSQuery(query []byte) ([]byte, error) {
	if len(query) < 12 {
		return nil, io.ErrUnexpectedEOF
	}
	questionEnd, name, qtype, err := parseDNSQuestion(query)
	if err != nil {
		return nil, err
	}
	response := make([]byte, 0, len(query)+128)
	response = append(response, query[:2]...)
	flags := uint16(0x8000 | 0x0080)
	if query[2]&0x01 != 0 {
		flags |= 0x0100
	}
	response = binary.BigEndian.AppendUint16(response, flags)
	response = binary.BigEndian.AppendUint16(response, 1)
	answerCountOffset := len(response)
	response = binary.BigEndian.AppendUint16(response, 0)
	response = binary.BigEndian.AppendUint16(response, 0)
	response = binary.BigEndian.AppendUint16(response, 0)
	response = append(response, query[12:questionEnd]...)

	var answers [][]byte
	if qtype == 28 {
		// The Drive transport and exit dialer are tuned around IPv4-first
		// target selection. Returning no AAAA records keeps VPN-mode apps from
		// opening literal IPv6 sockets that bypass the exit's family policy.
		return response, nil
	}
	if qtype == 1 {
		ips, lookupErr := net.LookupIP(strings.TrimSuffix(name, "."))
		if lookupErr != nil {
			response[3] = response[3]&0xf0 | 0x03
			return response, nil
		}
		for _, ip := range ips {
			if ip4 := ip.To4(); ip4 != nil {
				answers = append(answers, ip4)
			}
		}
	}
	for _, answer := range answers {
		response = append(response, 0xc0, 0x0c)
		response = binary.BigEndian.AppendUint16(response, qtype)
		response = binary.BigEndian.AppendUint16(response, 1)
		response = binary.BigEndian.AppendUint32(response, 60)
		response = binary.BigEndian.AppendUint16(response, uint16(len(answer)))
		response = append(response, answer...)
	}
	binary.BigEndian.PutUint16(response[answerCountOffset:answerCountOffset+2], uint16(len(answers)))
	return response, nil
}

func parseDNSQuestion(query []byte) (int, string, uint16, error) {
	if binary.BigEndian.Uint16(query[4:6]) == 0 {
		return 0, "", 0, fmt.Errorf("dns query has no questions")
	}
	var labels []string
	offset := 12
	for {
		if offset >= len(query) {
			return 0, "", 0, io.ErrUnexpectedEOF
		}
		labelLen := int(query[offset])
		offset++
		if labelLen == 0 {
			break
		}
		if labelLen&0xc0 != 0 {
			return 0, "", 0, fmt.Errorf("compressed dns question is not supported")
		}
		if offset+labelLen > len(query) {
			return 0, "", 0, io.ErrUnexpectedEOF
		}
		labels = append(labels, string(query[offset:offset+labelLen]))
		offset += labelLen
	}
	if offset+4 > len(query) {
		return 0, "", 0, io.ErrUnexpectedEOF
	}
	qtype := binary.BigEndian.Uint16(query[offset : offset+2])
	offset += 4
	return offset, strings.Join(labels, ".") + ".", qtype, nil
}

const (
	socksCommandConnect      = 0x01
	socksCommandUDPAssociate = 0x03
	socksCommandUDPInTCP     = 0x05
)
