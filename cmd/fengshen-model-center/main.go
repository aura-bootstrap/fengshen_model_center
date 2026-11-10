package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/modelmanifest"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeadapter"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimehttp"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
)

var buildVersion = "dev-local"

func main() {
	listen := flag.String("listen", "127.0.0.1:18120", "仅限本机回环监听地址")
	root := flag.String("root", "", "项目或受控安装根目录")
	rootKind := flag.String("root-kind", "development", "development 或 installed")
	data := flag.String("data", "", "独立路径登记目录")
	integrationData := flag.String("integration-data", "", "独立发现目录；空则不启用三端只读集成")
	installRoot := flag.String("install-root", "", "组件安装根目录")
	componentRoot := flag.String("component-root", "", "组件根目录；空则使用安装根下的 model-center/components")
	info := flag.Bool("component-info", false, "仅输出组件元数据，不扫描或加载模型")
	haarInfo := flag.Bool("haar-component-info", false, "仅输出内嵌 Haar 组件元数据，不扫描或加载模型")
	writeManifest := flag.String("write-model-manifest", "", "从分号分隔的模型根目录生成用户模型清单并退出")
	flag.Parse()
	haarInfoRequested := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "haar-component-info" {
			haarInfoRequested = true
		}
	})
	if haarInfoRequested {
		if !*haarInfo || len(os.Args) != 2 || os.Args[1] != "--haar-component-info" {
			log.Fatal("Haar 元数据入口仅接受单参数 --haar-component-info")
		}
		writeHaarComponentInfo(os.Stdout)
		return
	}
	if *info {
		writeComponentInfo(os.Stdout)
		return
	}
	if *writeManifest != "" {
		if err := writeModelManifest(splitRoots(*writeManifest)); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(*listen, *root, *rootKind, *data, *integrationData, *installRoot, *componentRoot); err != nil {
		log.Fatal(err)
	}
}

func run(listen, root, rootKind, data, integrationData, installRoot, componentRoot string) error {
	if err := validateListen(listen); err != nil {
		return err
	}
	resolved, err := resolveStartupRoot(root, rootKind)
	if err != nil {
		return err
	}
	if data == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		data = filepath.Join(base, "FengshenSlicer", "model-center")
	}
	manifestPath := ""
	if home, err := os.UserHomeDir(); err == nil {
		manifestPath = modelmanifest.DefaultPath(home)
	}
	manifest, raw, manifestErr := modelmanifest.Load(manifestPath)
	manifestRevision := modelmanifest.Revision(raw)
	manifestError := ""
	if manifestErr != nil {
		manifestError = manifestErr.Error()
	}
	if installRoot == "" {
		installRoot = resolved
	}
	manager, err := newManagerWithInstallRoot(resolved, data, manifest, manifestRevision, manifestError, manifestPath, rootKind, installRoot, componentRoot)
	if err != nil {
		return err
	}
	defer manager.Close()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	address := listener.Addr().String()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var host *integration.Host
	if integrationData != "" {
		if installRoot == "" {
			installRoot = resolved
		}
		host, err = integration.NewHost(integration.HostConfig{
			Config:  integration.Config{InstallRoot: installRoot, Root: resolved, RootKind: rootKind, DataDir: integrationData},
			Address: address, Build: buildVersion,
			Health: func() integration.Health {
				state := "ready"
				switch manager.State().StorageError {
				case "":
				case "registration_unavailable", "registration_changed":
					state = "degraded"
				default:
					state = "read_only"
				}
				return integration.Health{State: state}
			},
			Dependencies:        manager.dependencySnapshot,
			Dashboard:           manager.dashboardSnapshot,
			Report:              manager.reportSnapshot,
			ModelFiles:          manager.modelFilesPage,
			RuntimeCapabilities: manager.runtimeCapabilities,
		})
		if err != nil {
			return err
		}
		if err = host.Register(); err != nil {
			return err
		}
		defer host.Close()
	}
	store, err := runtimeassets.New(data)
	if err != nil {
		return err
	}
	executor, err := runtimeadapter.New(manager.resolveRuntimeBinding, store, data)
	if err != nil {
		store.Close()
		return err
	}
	jobs, err := runtimejobs.New(executor, runtimejobs.Options{})
	if err != nil {
		executor.Close()
		store.Close()
		return err
	}
	manager.runtimeHandler = runtimehttp.New(store, jobs, manager.validateRuntimeBinding)
	server := &http.Server{Handler: newHandler(manager, address, host), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		manager.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("本地模型中心服务 http://%s · 根目录 %s", address, resolved)
	err = server.Serve(listener)
	stop()
	if host != nil {
		_ = host.Close()
	}
	<-shutdownDone
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func validateListen(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("管理中心仅允许监听字面量回环 IP 地址")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("监听端口无效")
	}
	return nil
}

func resolveRoot(path string) (string, error) {
	if path == "" {
		return os.Getwd()
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("项目根目录无效")
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		return "", fmt.Errorf("项目根目录不存在")
	}
	return absolute, nil
}
