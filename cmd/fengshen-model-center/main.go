package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
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

//go:embed web/index.html web/styles.css web/app.js
var assets embed.FS

var buildVersion = "dev-local"

func main() {
	listen := flag.String("listen", "127.0.0.1:18120", "仅限本机回环监听地址")
	root := flag.String("root", "", "项目或受控安装根目录")
	rootKind := flag.String("root-kind", "development", "development 或 installed")
	data := flag.String("data", "", "独立路径登记目录")
	integrationData := flag.String("integration-data", "", "独立可信发现目录；空则不启用三端集成")
	installRoot := flag.String("install-root", "", "可信引擎安装根目录")
	centerSHA := flag.String("center-sha256", "", "可信启动配置中的中心摘要")
	deliverySHA := flag.String("delivery-sha256", "", "可信启动配置中的交付摘要")
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
	if err := run(*listen, *root, *rootKind, *data, *integrationData, *installRoot, *centerSHA, *deliverySHA); err != nil {
		log.Fatal(err)
	}
}

func run(listen, root, rootKind, data, integrationData, installRoot, centerSHA, deliverySHA string) error {
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
	manager, err := newManagerWithInstallRoot(resolved, data, manifest, manifestRevision, manifestError, manifestPath, rootKind, installRoot)
	if err != nil {
		return err
	}
	defer manager.Close()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	address := listener.Addr().String()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var host *integration.Host
	if integrationData != "" {
		if installRoot == "" {
			installRoot = resolved
		}
		allowlist, loadErr := loadStartupEngineTrust(ctx, installRoot, centerSHA, deliverySHA)
		if loadErr != nil {
			return loadErr
		}
		host, err = integration.NewHost(integration.HostConfig{
			Config:          integration.Config{InstallRoot: installRoot, Root: resolved, RootKind: rootKind, DataDir: integrationData, CenterSHA256: centerSHA, DeliverySHA256: deliverySHA},
			EngineAllowlist: allowlist,
			CancelSession: func(grant integration.SessionGrant) {
				if manager.runtimeHandler != nil {
					manager.runtimeHandler.ExpireSession(grant)
				}
			},
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
				return integration.Health{UIAssetsReady: true, State: state}
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
		if len(allowlist) > 0 {
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
			manager.runtimeHandler = runtimehttp.New(host, store, jobs, manager.validateRuntimeBinding)
		}
	}
	server := &http.Server{Handler: newHandler(manager, hex.EncodeToString(tokenBytes), address, host), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ipcDone := make(chan struct{})
	if host != nil {
		go func() {
			defer close(ipcDone)
			if err := host.ServeIPC(ctx); err != nil && ctx.Err() == nil {
				log.Print("可信集成通道已停止")
			}
		}()
	} else {
		close(ipcDone)
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		manager.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("本地模型管理中心 http://%s · 根目录 %s", address, resolved)
	err = server.Serve(listener)
	stop()
	if host != nil {
		_ = host.Close()
	}
	<-ipcDone
	<-shutdownDone
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func loadStartupEngineTrust(ctx context.Context, root, centerSHA, deliverySHA string) ([]integration.TrustedEngine, error) {
	if centerSHA == "" && deliverySHA == "" {
		if _, err := os.Lstat(filepath.Join(root, "model-center", "engine-trust.json")); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errors.New("center_engine_trust_unconfirmed")
	}
	engines, err := integration.LoadEngineTrust(ctx, root, centerSHA, deliverySHA)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("center_engine_trust_invalid")
	}
	return engines, nil
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
	if path != "" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		if !validProject(absolute) {
			return "", fmt.Errorf("项目根目录无效，缺少 sidecar 或 models 目录")
		}
		return absolute, nil
	}
	path, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if validProject(path) {
			return path, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("请从项目目录启动，或使用 -root 指定项目根目录")
		}
		path = parent
	}
}
