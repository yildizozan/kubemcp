package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	clientset *kubernetes.Clientset
	registry  = prometheus.NewRegistry()

	activeConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mcp_active_connections",
		Help: "Number of active MCP connections",
	})

	requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mcp_requests_total",
		Help: "Total number of MCP requests",
	}, []string{"endpoint"})
)

func init() {
	registry.MustRegister(activeConnections)
	registry.MustRegister(requestsTotal)
}

func main() {
	var config *rest.Config
	var err error

	// Önce in-cluster config deneyelim
	config, err = rest.InClusterConfig()
	if err != nil {
		log.Println("In-cluster config bulunamadı, kubeconfig dosyası deneniyor...")

		// KUBECONFIG environment variable'ını kontrol et
		kubeconfigPath := os.Getenv("KUBECONFIG")
		if kubeconfigPath == "" {
			// Varsayılan home directory kubeconfig path
			homeDir, err := os.UserHomeDir()
			if err != nil {
				panic(fmt.Sprintf("Home directory bulunamadı: %v", err))
			}
			kubeconfigPath = filepath.Join(homeDir, ".kube", "config")
		}

		// Kubeconfig dosyasından config yükle
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			panic(fmt.Sprintf("Kubeconfig yüklenemedi: %v", err))
		}
		log.Printf("Kubeconfig kullanılıyor: %s", kubeconfigPath)
	} else {
		log.Println("In-cluster config kullanılıyor")
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(fmt.Sprintf("Kubernetes clientset oluşturulamadı: %v", err))
	}

	s := server.NewMCPServer(
		"Kubernetes MCP Server",
		"0.0.1",
		server.WithToolCapabilities(true),
	)

	// Create SSE server for MCP with options
	sseServer := server.NewSSEServer(s,
		server.WithSSEEndpoint("/sse"),
		server.WithMessageEndpoint("/message"),
	)

	toolGetPodDetails := mcp.NewTool("get_pod_details",
		mcp.WithDescription("Pod detaylarını (spec, status) getirir"),
		mcp.WithString("podName",
			mcp.Description("Pod ismi"),
			mcp.Required(),
		),
		mcp.WithString("namespace",
			mcp.Description("Namespace (varsayılan: default)"),
			mcp.DefaultString("default"),
		),
	)

	toolPodsGetByLabel := mcp.NewTool("get_pods_by_label",
		mcp.WithDescription("Belirtilen label'a sahip podları getirir"),
		mcp.WithString("labelSelector",
			mcp.Description("Label seçici (örneğin: app=nginx veya app.kubernetes.io/instance=nginx)"),
			mcp.Required(),
		),
	)

	s.AddTool(toolGetPodDetails, getPodDatailsHandler)
	s.AddTool(toolPodsGetByLabel, getPodByLabelHandler)

	// Setup HTTP mux
	mux := http.NewServeMux()

	// Add Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	// Add SSE endpoints for MCP with connection tracking
	mux.Handle("/sse", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeConnections.Inc()
		defer activeConnections.Dec()
		requestsTotal.WithLabelValues("sse").Inc()
		sseServer.SSEHandler().ServeHTTP(w, r)
	}))

	mux.Handle("/message", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsTotal.WithLabelValues("message").Inc()
		sseServer.MessageHandler().ServeHTTP(w, r)
	}))

	// Add root endpoint for MCP (LibreChat might try this)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			activeConnections.Inc()
			defer activeConnections.Dec()
			requestsTotal.WithLabelValues("root").Inc()
			sseServer.SSEHandler().ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
	}))

	// Start HTTP server
	addr := ":8080"
	log.Printf("Starting Kubernetes MCP Server on %s...", addr)
	log.Printf("SSE endpoint: :%s/sse", addr)
	log.Printf("Message endpoint: :%s/message", addr)
	log.Printf("Metrics endpoint: :%s/metrics", addr)
	log.Printf("Root endpoint (MCP): :%s/", addr)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func deleteUnnecessaryFieldsFromPodSpec(pod *v1.Pod) {
	pod.ManagedFields = nil

	// Annotations map olduğu için delete kullanın
	delete(pod.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
	delete(pod.Annotations, "kubernetes.io/psp")
}

func getPodByLabelHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("Invalid arguments format"), nil
	}

	labelSelector, _ := args["labelSelector"].(string)
	if labelSelector == "" {
		return mcp.NewToolResultError("labelSelector is required"), nil
	}

	log.Println(labelSelector)

	listOptions := metav1.ListOptions{
		LabelSelector: labelSelector,
	}
	pods, err := clientset.CoreV1().Pods("").List(ctx, listOptions)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Error getting pods list: %v", err)), nil
	}

	log.Printf("Found %d pods", len(pods.Items))

	for i := range pods.Items {
		deleteUnnecessaryFieldsFromPodSpec(&pods.Items[i])
	}

	return mcp.NewToolResultJSON(pods.Items)
}

func getPodDatailsHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("Invalid arguments format"), nil
	}

	podName, _ := args["podName"].(string)
	namespace, _ := args["namespace"].(string)

	if namespace == "" {
		namespace = "default"
	}

	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Pod not found: %v", err)), nil
	}

	deleteUnnecessaryFieldsFromPodSpec(pod)

	return mcp.NewToolResultJSON(pod)
}
