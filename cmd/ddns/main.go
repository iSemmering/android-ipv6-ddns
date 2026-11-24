package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"ipv6-ddns/internal/android"
	"ipv6-ddns/internal/app"
	"ipv6-ddns/internal/dns/huawei"
)

const stateDirectory = "/data/adb/ipv6-ddns"

func main() {
	if err := mainCommand(); err != nil {
		if len(os.Args) == 2 && os.Args[1] == "request" {
			_ = json.NewEncoder(os.Stdout).Encode(app.Reply{Error: err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func mainCommand() error {
	if len(os.Args) != 2 {
		return errors.New("缺少 IPv6 DDNS 模块命令")
	}
	if os.Geteuid() != 0 {
		return errors.New("请通过 KernelSU 管理器运行")
	}
	dir := stateDirectory
	if isolated := os.Getenv("DDNS_STATE_DIR"); isolated != "" {
		dir = isolated
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("无法创建模块的私有数据目录")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return errors.New("无法限制模块数据目录权限")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	module := filepath.Dir(filepath.Dir(exe))
	switch os.Args[1] {
	case "daemon":
		return serve(dir, module)
	case "start":
		return startDaemon(exe, module)
	case "request":
		return request(dir, exe, module)
	case "stop":
		return stop(dir)
	default:
		return errors.New("未知的 IPv6 DDNS 模块命令")
	}
}

func blocked(module string) bool {
	for _, name := range []string{"disable", "remove"} {
		if _, err := os.Stat(filepath.Join(module, name)); err == nil {
			return true
		}
	}
	return false
}

func startDaemon(exe, module string) error {
	if blocked(module) {
		return errors.New("请先在 KernelSU 管理器中启用模块")
	}
	command := exec.Command(exe, "daemon")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return errors.New("无法启动 IPv6 DDNS 后台进程")
	}
	return command.Process.Release()
}

func socketPath(dir string) string {
	return filepath.Join(dir, "control.sock")
}

func request(dir, exe, module string) error {
	if blocked(module) {
		return errors.New("请先在 KernelSU 管理器中启用模块")
	}
	encoded := os.Getenv("DDNS_REQUEST")
	_ = os.Unsetenv("DDNS_REQUEST")
	if len(encoded) == 0 || len(encoded) > 10924 {
		return errors.New("WebUI 请求为空或过长")
	}
	input, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(input) > 8192 {
		return errors.New("WebUI 请求编码无效")
	}
	var payload app.Request
	if err := json.Unmarshal(input, &payload); err != nil || payload.Op == "shutdown" {
		return errors.New("WebUI 请求格式错误")
	}
	socket := socketPath(dir)
	conn, err := net.DialTimeout("unix", socket, 300*time.Millisecond)
	if err != nil {
		if err := startDaemon(exe, module); err != nil {
			return err
		}
		for range 35 {
			time.Sleep(100 * time.Millisecond)
			conn, err = net.DialTimeout("unix", socket, 150*time.Millisecond)
			if err == nil {
				break
			}
		}
	}
	if err != nil {
		return errors.New("无法连接后台服务，请重启手机后再试")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(40 * time.Second))
	if err := json.NewEncoder(conn).Encode(payload); err != nil {
		return errors.New("无法向后台服务提交请求")
	}
	var reply app.Reply
	if err := json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&reply); err != nil {
		return errors.New("后台响应超时或无法读取")
	}
	return json.NewEncoder(os.Stdout).Encode(reply)
}

func serve(dir, module string) error {
	if blocked(module) {
		return nil
	}
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	engine, err := app.New(dir, android.Network{}, huawei.New(android.HTTPClient()), android.Timezone(ctx))
	if err != nil {
		return err
	}
	socket := socketPath(dir)
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	moduleProp := filepath.Join(module, "module.prop")
	_, propErr := os.Stat(moduleProp)
	if propErr == nil {
		engine.SetModuleCard(moduleProp)
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = listener.Close()
				return
			case <-ticker.C:
				_, err := os.Stat(moduleProp)
				if blocked(module) || (propErr == nil && errors.Is(err, os.ErrNotExist)) {
					cancel()
				}
			}
		}
	}()
	var work sync.WaitGroup
	work.Add(1)
	go func() {
		defer work.Done()
		engine.Run(ctx)
	}()
	for ctx.Err() == nil {
		conn, err := listener.AcceptUnix()
		if err != nil {
			break
		}
		work.Add(1)
		go func() {
			defer work.Done()
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(40 * time.Second))
			var payload app.Request
			if err := json.NewDecoder(io.LimitReader(conn, 8192)).Decode(&payload); err != nil {
				_ = json.NewEncoder(conn).Encode(app.Reply{Error: "后台请求格式错误"})
				return
			}
			if payload.Op == "shutdown" {
				_ = json.NewEncoder(conn).Encode(app.Reply{OK: true})
				cancel()
				return
			}
			requestCtx, finish := context.WithTimeout(ctx, 35*time.Second)
			defer finish()
			_ = json.NewEncoder(conn).Encode(engine.Call(requestCtx, payload))
		}()
	}
	cancel()
	work.Wait()
	return nil
}

func stop(dir string) error {
	socket := socketPath(dir)
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_ = json.NewEncoder(conn).Encode(app.Request{Op: "shutdown"})
		var ack app.Reply
		_ = json.NewDecoder(conn).Decode(&ack)
		_ = conn.Close()
	}
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for range 100 {
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("后台服务尚未完全停止")
}
