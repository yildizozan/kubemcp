package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(fmt.Sprintf("In-Cluster config yüklenemedi: %v", err))
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

	toolGetByLabel := mcp.NewTool("get_pods_by_label",
		mcp.WithDescription("Belirtilen label'a sahip podları getirir"),
		mcp.WithString("labelSelector",
			mcp.Description("Label seçici (örneğin: app=nginx veya app.kubernetes.io/instance=nginx)"),
			mcp.Required(),
		),
	)

	s.AddTool(toolGetPodDetails, getPodDatailsHandler)
	s.AddTool(toolGetByLabel, getByLabelHandler)

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

func getByLabelHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("Invalid arguments format"), nil
	}

	labelSelector, _ := args["labelSelector"].(string)

	listOptions := metav1.ListOptions{
		LabelSelector: labelSelector,
	}
	pods, err := clientset.CoreV1().Pods("").List(ctx, listOptions)
	if err != nil {
		return mcp.NewToolResultError("Error getting pods list"), nil
	}

	// Delete noisy fields like ManagedFields
	for _, pod := range pods.Items {
		pod.ManagedFields = nil
	}

	jsonData, err := json.MarshalIndent(pods.Items, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("Unmarshall Error"), nil
	}

	result, err := mcp.NewToolResultJSON(jsonData)
	if err != nil {
		return nil, err
	}
	return result, nil
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

	// K8s API Çağrısı
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Pod not found: %v", err)), nil
	}

	// ManagedFields gibi gürültülü verileri temizle
	pod.ManagedFields = nil

	// JSON'a çevir
	jsonData, err := json.MarshalIndent(pod, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("Unmarshall Error"), nil
	}

	result, err := mcp.NewToolResultJSON(jsonData)
	if err != nil {
		return nil, err
	}
	return result, nil
}
