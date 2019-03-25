package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/client"
	"golang.org/x/net/dns/dnsmessage"
)

func main() {
	var bindIP, nameSuffix string
	flag.StringVar(&bindIP, "bind", "127.0.0.127", "ip to bind")
	flag.StringVar(&nameSuffix, "suffix", "docker", "domain name suffix")
	flag.Parse()
	dockerClient, err := client.NewEnvClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "can't connect to docker:", err)
		os.Exit(-1)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 53})
	if err != nil {
		fmt.Fprintln(os.Stderr, "can't open socket:", err)
		os.Exit(-2)
	}
	defer conn.Close()
	b := make([]byte, 4096)
	for {
		n, addr, err := conn.ReadFromUDP(b)
		if err != nil {
			fmt.Fprintln(os.Stderr, "socket read error:", err)
			continue
		}
		go func(m []byte, addr *net.UDPAddr, cl *client.Client) {
			msg, err := replyDNS(m, cl, nameSuffix)
			if err != nil {
				fmt.Fprintln(os.Stderr, "can't create reply:", err)
				return
			}
			rb, err := msg.Pack()
			if err != nil {
				fmt.Fprintln(os.Stderr, "can't pack message:", err)
				return
			}
			if _, err = conn.WriteToUDP(rb, addr); err != nil {
				fmt.Fprintln(os.Stderr, "can't write to socket:", err)
				return
			}
		}(b[:n], addr, dockerClient)
	}
}

func replyDNS(msg []byte, cl *client.Client, suffix string) (*dnsmessage.Message, error) {
	r := &dnsmessage.Message{}
	if err := r.Unpack(msg); err != nil {
		return nil, err
	}
	if r.Header.Response {
		return nil, fmt.Errorf("go a response instead of a query")
	}
	if len(r.Questions) < 1 {
		return nil, fmt.Errorf("no questions")
	}
	r.RecursionAvailable = false
	r.RecursionDesired = false
	r.Response = true
	r.Questions = r.Questions[:1]
	q := r.Questions[0]
	if q.Type != dnsmessage.TypeA || q.Class != dnsmessage.ClassINET {
		r.RCode = dnsmessage.RCodeNameError
		return r, nil
	}
	cn := string(q.Name.Data[:q.Name.Length])
	if !strings.HasSuffix(cn, "."+suffix+".") {
		r.RCode = dnsmessage.RCodeNameError
		return r, nil
	}
	ip, err := resolveContainerName(cl, strings.TrimSuffix(cn, "."+suffix+"."))
	if err != nil {
		if !client.IsErrNotFound(err) {
			return nil, err
		}
		r.RCode = dnsmessage.RCodeNameError
		return r, nil
	}
	r.RCode = dnsmessage.RCodeSuccess
	r.Answers = append(make([]dnsmessage.Resource, 0, 1), dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name:  q.Name,
			Type:  q.Type,
			Class: q.Class,
			TTL:   60,
		},
		Body: &dnsmessage.AResource{A: ip},
	})
	return r, nil
}

func resolveContainerName(cl *client.Client, name string) ([4]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	info, err := cl.ContainerInspect(ctx, name)
	if err != nil {
		return [4]byte{}, err
	}
	netInfo, ok := info.NetworkSettings.Networks[string(info.HostConfig.NetworkMode)]
	if !ok {
		for _, netInfo = range info.NetworkSettings.Networks {
			ok = true
			break
		}
		if !ok {
			return [4]byte{}, fmt.Errorf("error getting network info for %s", name)
		}
	}
	ip := net.ParseIP(netInfo.IPAddress).To4()
	if len(ip) == 0 {
		return [4]byte{}, errors.New("can't get IP address")
	}
	return [4]byte{ip[0], ip[1], ip[2], ip[3]}, nil
}
