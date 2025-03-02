package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/gorilla/mux"
)

// Configuration structure
type Config struct {
	Mode           string         `json:"mode"`           // "all" or "manual"
	PowerLimit     uint32         `json:"powerLimit"`     // Default power limit in watts for "all" mode
	ManualLimits   map[int]uint32 `json:"manualLimits"`   // GPU index to power limit map for "manual" mode
	APIKey         string         `json:"apiKey"`         // API key for authentication
	APIPort        int            `json:"apiPort"`        // Port for API server, default 8080
	StartAPIServer bool           `json:"startAPIServer"` // Whether to start the API server
}

// GPU information structure
type GPUInfo struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	PowerLimit uint32 `json:"powerLimit"`      // Current power limit in watts
	MinLimit   uint32 `json:"minLimit"`        // Minimum allowed power limit in watts
	MaxLimit   uint32 `json:"maxLimit"`        // Maximum allowed power limit in watts
	PowerUsage uint32 `json:"powerUsage"`      // Current power usage in watts
	Supported  bool   `json:"powerManagement"` // Whether power management is supported
}

// Power limit update request
type PowerLimitRequest struct {
	Mode         string         `json:"mode"`         // "all" or "manual"
	PowerLimit   uint32         `json:"powerLimit"`   // Power limit for all GPUs in watts
	ManualLimits map[int]uint32 `json:"manualLimits"` // GPU index to power limit map
}

// Global variables for API access
var gpuCache []GPUInfo
var config Config

// Print help information
func printHelp() {
	fmt.Println("NVIDIA Power Control - Manage power limits for NVIDIA GPUs")
	fmt.Println("\nUsage:")
	fmt.Println("  Set power limit for all GPUs:")
	fmt.Println("    nvidia-power-control <power_limit_in_watts>")
	fmt.Println("\n  Set power limit for specific GPUs:")
	fmt.Println("    nvidia-power-control --gpu=0:<power_limit> --gpu=1:<power_limit> ...")
	fmt.Println("\n  Run in API server mode (requires config.json):")
	fmt.Println("    nvidia-power-control")
	fmt.Println("\nExamples:")
	fmt.Println("  Set all GPUs to 200 watts:")
	fmt.Println("    nvidia-power-control 200")
	fmt.Println("\n  Set GPU 0 to 200 watts and GPU 1 to 180 watts:")
	fmt.Println("    nvidia-power-control --gpu=0:200 --gpu=1:180")
	fmt.Println("\nConfig.json format (for API server mode):")
	fmt.Println(`  {
    "mode": "all",                   // "all" or "manual"
    "powerLimit": 250,               // Power limit in watts for "all" mode
    "manualLimits": {                // For "manual" mode
      "0": 220,                      // GPU index : power limit in watts
      "1": 180
    },
    "apiKey": "your-secure-api-key", // Required for API server
    "apiPort": 8080,                 // Optional, defaults to 8080
    "startAPIServer": true           // Whether to start the API server (true/false)
  }`)
}

// Initialize NVML and get GPU information
func initNVML() error {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to initialize NVML: %v", nvml.ErrorString(ret))
	}

	// Get number of GPUs
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to get device count: %v", nvml.ErrorString(ret))
	}

	// Reset the GPU cache
	gpuCache = make([]GPUInfo, count)

	// Populate GPU cache with information
	for i := 0; i < count; i++ {
		gpuInfo, err := getGPUInfo(i)
		if err != nil {
			log.Printf("Warning: Failed to get info for GPU %d: %v", i, err)
			continue
		}
		gpuCache[i] = gpuInfo
	}

	return nil
}

// Get information for a specific GPU
func getGPUInfo(index int) (GPUInfo, error) {
	var info GPUInfo
	info.Index = index

	// Get device handle
	device, ret := nvml.DeviceGetHandleByIndex(index)
	if ret != nvml.SUCCESS {
		return info, fmt.Errorf("failed to get handle: %v", nvml.ErrorString(ret))
	}

	// Get GPU name
	name, ret := nvml.DeviceGetName(device)
	if ret != nvml.SUCCESS {
		name = "Unknown"
	}
	info.Name = name

	// Check if power management is supported
	mode, ret := nvml.DeviceGetPowerManagementMode(device)
	if ret != nvml.SUCCESS {
		return info, fmt.Errorf("failed to get power management mode: %v", nvml.ErrorString(ret))
	}
	info.Supported = (mode == nvml.FEATURE_ENABLED)

	if !info.Supported {
		return info, nil // Return early with limited info if not supported
	}

	// Get current power limit
	currentLimit, ret := nvml.DeviceGetPowerManagementLimit(device)
	if ret != nvml.SUCCESS {
		return info, fmt.Errorf("failed to get current power limit: %v", nvml.ErrorString(ret))
	}
	info.PowerLimit = currentLimit / 1000 // Convert to watts

	// Get power limit constraints
	minLimit, maxLimit, ret := nvml.DeviceGetPowerManagementLimitConstraints(device)
	if ret != nvml.SUCCESS {
		return info, fmt.Errorf("failed to get power limit constraints: %v", nvml.ErrorString(ret))
	}
	info.MinLimit = minLimit / 1000 // Convert to watts
	info.MaxLimit = maxLimit / 1000 // Convert to watts

	// Get current power usage
	power, ret := nvml.DeviceGetPowerUsage(device)
	if ret == nvml.SUCCESS {
		info.PowerUsage = power / 1000 // Convert to watts
	}

	return info, nil
}

// Set power limit for a specific GPU
func setPowerLimit(index int, limitWatts uint32) (GPUInfo, error) {
	// Get device handle
	device, ret := nvml.DeviceGetHandleByIndex(index)
	if ret != nvml.SUCCESS {
		return GPUInfo{}, fmt.Errorf("failed to get handle: %v", nvml.ErrorString(ret))
	}

	// Check if power management is supported
	mode, ret := nvml.DeviceGetPowerManagementMode(device)
	if ret != nvml.SUCCESS {
		return GPUInfo{}, fmt.Errorf("failed to get power management mode: %v", nvml.ErrorString(ret))
	}
	if mode != nvml.FEATURE_ENABLED {
		return GPUInfo{}, fmt.Errorf("power management not supported")
	}

	// Get power limit constraints
	minLimit, maxLimit, ret := nvml.DeviceGetPowerManagementLimitConstraints(device)
	if ret != nvml.SUCCESS {
		return GPUInfo{}, fmt.Errorf("failed to get power limit constraints: %v", nvml.ErrorString(ret))
	}

	// Convert watts to milliwatts
	limitMW := limitWatts * 1000

	// Clamp to allowed range
	if limitMW < minLimit {
		limitMW = minLimit
		log.Printf("GPU %d: Desired limit %d W below minimum %d W, setting to %d W",
			index, limitWatts, minLimit/1000, limitMW/1000)
	} else if limitMW > maxLimit {
		limitMW = maxLimit
		log.Printf("GPU %d: Desired limit %d W above maximum %d W, setting to %d W",
			index, limitWatts, maxLimit/1000, limitMW/1000)
	}

	// Set the new power limit
	ret = nvml.DeviceSetPowerManagementLimit(device, limitMW)
	if ret != nvml.SUCCESS {
		return GPUInfo{}, fmt.Errorf("failed to set power limit: %v", nvml.ErrorString(ret))
	}

	// Get updated GPU info after change
	return getGPUInfo(index)
}

// API middleware for authentication
func apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")

		// Check if API key is valid
		if apiKey != config.APIKey {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid API key"})
			return
		}

		// Call the next handler
		next.ServeHTTP(w, r)
	})
}

// API handler to get all GPU information
func getGPUsHandler(w http.ResponseWriter, r *http.Request) {
	// Initialize NVML to get fresh data
	err := initNVML()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(gpuCache)
}

// API handler to get a specific GPU's information
func getGPUHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	indexStr := vars["index"]

	index, err := strconv.Atoi(indexStr)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid GPU index"})
		return
	}

	// Initialize NVML to get fresh data
	err = initNVML()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if index < 0 || index >= len(gpuCache) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "GPU index out of range"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(gpuCache[index])
}

// API handler to set power limits
func setPowerLimitsHandler(w http.ResponseWriter, r *http.Request) {
	var request PowerLimitRequest
	err := json.NewDecoder(r.Body).Decode(&request)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request format"})
		return
	}

	// Get number of GPUs
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("Failed to get device count: %v", nvml.ErrorString(ret))})
		return
	}

	// Process based on mode
	var updatedGPUs []GPUInfo

	if request.Mode == "all" {
		// Set the same power limit for all GPUs
		for i := 0; i < count; i++ {
			updatedInfo, err := setPowerLimit(i, request.PowerLimit)
			if err != nil {
				log.Printf("GPU %d: Failed to set power limit: %v", i, err)
				continue
			}
			updatedGPUs = append(updatedGPUs, updatedInfo)
		}
	} else if request.Mode == "manual" {
		// Set specific power limits for specified GPUs
		for gpuIndex, powerLimit := range request.ManualLimits {
			if gpuIndex >= 0 && gpuIndex < count {
				updatedInfo, err := setPowerLimit(gpuIndex, powerLimit)
				if err != nil {
					log.Printf("GPU %d: Failed to set power limit: %v", gpuIndex, err)
					continue
				}
				updatedGPUs = append(updatedGPUs, updatedInfo)
			} else {
				log.Printf("Warning: GPU %d specified in request doesn't exist", gpuIndex)
			}
		}
	} else {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid mode (must be 'all' or 'manual')"})
		return
	}

	// Update the GPU cache with new information
	err = initNVML()
	if err != nil {
		log.Printf("Warning: Failed to refresh GPU cache: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updatedGPUs)
}

// Start the API server
func startAPIServer() {
	router := mux.NewRouter()

	// Apply middleware to all routes
	api := router.PathPrefix("/api").Subrouter()
	api.Use(apiKeyMiddleware)

	// Define API routes
	api.HandleFunc("/gpus", getGPUsHandler).Methods("GET")
	api.HandleFunc("/gpus/{index}", getGPUHandler).Methods("GET")
	api.HandleFunc("/power", setPowerLimitsHandler).Methods("POST")

	// Start server
	port := config.APIPort
	if port == 0 {
		port = 8080 // Default port
	}

	log.Printf("Starting API server on port %d", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), router))
}

// Load configuration from file
func loadConfig() (Config, error) {
	// Set default values
	config := Config{
		Mode:           "all",
		PowerLimit:     250,
		APIPort:        8080,
		StartAPIServer: false, // Default to not starting API server
	}

	// Try to load config file
	configData, err := ioutil.ReadFile("config.json")
	if err != nil {
		return config, fmt.Errorf("no config.json found: %v", err)
	}

	// Parse config file
	err = json.Unmarshal(configData, &config)
	if err != nil {
		return config, fmt.Errorf("failed to parse config.json: %v", err)
	}

	return config, nil
}

// Apply power settings from config
func applyConfigSettings(config Config, count int) {
	if config.Mode == "all" {
		// Apply same power limit to all GPUs
		fmt.Printf("Setting all GPUs to %d watts\n", config.PowerLimit)
		for i := 0; i < count; i++ {
			gpuInfo, err := setPowerLimit(i, config.PowerLimit)
			if err != nil {
				fmt.Printf("GPU %d: Failed to set power limit: %v\n", i, err)
				continue
			}
			fmt.Printf("GPU %d (%s): Power limit set to %d W\n",
				gpuInfo.Index, gpuInfo.Name, gpuInfo.PowerLimit)
		}
	} else if config.Mode == "manual" {
		// Apply specific power limits
		for gpuIndex, powerLimit := range config.ManualLimits {
			if gpuIndex >= 0 && gpuIndex < count {
				gpuInfo, err := setPowerLimit(gpuIndex, powerLimit)
				if err != nil {
					fmt.Printf("GPU %d: Failed to set power limit: %v\n", gpuIndex, err)
					continue
				}
				fmt.Printf("GPU %d (%s): Power limit set to %d W\n",
					gpuInfo.Index, gpuInfo.Name, gpuInfo.PowerLimit)
			} else {
				fmt.Printf("Warning: GPU %d specified in config doesn't exist\n", gpuIndex)
			}
		}
	} else {
		fmt.Printf("Invalid mode in config: %s (must be 'all' or 'manual')\n", config.Mode)
	}
}

// Parse GPU specific command line parameter (--gpu=<index>:<limit>)
func parseGPUParam(param string) (int, uint32, error) {
	parts := strings.Split(param, "=")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "--gpu") {
		return -1, 0, fmt.Errorf("invalid parameter format: %s", param)
	}

	gpuParts := strings.Split(parts[1], ":")
	if len(gpuParts) != 2 {
		return -1, 0, fmt.Errorf("invalid GPU parameter: %s (expected --gpu=index:limit)", param)
	}

	index, err := strconv.Atoi(gpuParts[0])
	if err != nil {
		return -1, 0, fmt.Errorf("invalid GPU index: %s", gpuParts[0])
	}

	limit, err := strconv.ParseUint(gpuParts[1], 10, 32)
	if err != nil {
		return -1, 0, fmt.Errorf("invalid power limit: %s", gpuParts[1])
	}

	return index, uint32(limit), nil
}

func main() {
	// Initialize NVML first
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		fmt.Printf("Failed to initialize NVML: %v\n", nvml.ErrorString(ret))
		os.Exit(1)
	}
	defer nvml.Shutdown()

	// Get number of GPUs
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		fmt.Printf("Failed to get device count: %v\n", nvml.ErrorString(ret))
		os.Exit(1)
	}

	// Check command line arguments
	if len(os.Args) > 1 {
		// Command line mode - process arguments

		// Check for GPU-specific parameters
		if strings.HasPrefix(os.Args[1], "--gpu") {
			// Process each --gpu parameter
			for _, arg := range os.Args[1:] {
				if strings.HasPrefix(arg, "--gpu") {
					index, limit, err := parseGPUParam(arg)
					if err != nil {
						fmt.Println(err)
						printHelp()
						os.Exit(1)
					}

					if index >= 0 && index < count {
						gpuInfo, err := setPowerLimit(index, limit)
						if err != nil {
							fmt.Printf("GPU %d: Failed to set power limit: %v\n", index, err)
							continue
						}
						fmt.Printf("GPU %d (%s): Power limit set to %d W\n",
							gpuInfo.Index, gpuInfo.Name, gpuInfo.PowerLimit)
					} else {
						fmt.Printf("Error: GPU %d doesn't exist\n", index)
					}
				}
			}
		} else {
			// Set the same limit for all GPUs
			desiredW, err := strconv.ParseUint(os.Args[1], 10, 32)
			if err != nil || desiredW == 0 {
				fmt.Printf("Invalid power limit: %s (must be a positive integer)\n", os.Args[1])
				printHelp()
				os.Exit(1)
			}

			fmt.Printf("Setting all GPUs to %d watts\n", desiredW)
			for i := 0; i < count; i++ {
				gpuInfo, err := setPowerLimit(i, uint32(desiredW))
				if err != nil {
					fmt.Printf("GPU %d: Failed to set power limit: %v\n", i, err)
					continue
				}
				fmt.Printf("GPU %d (%s): Power limit set to %d W\n",
					gpuInfo.Index, gpuInfo.Name, gpuInfo.PowerLimit)
			}
		}
	} else {
		// No command line arguments - check for config.json
		cfg, err := loadConfig()
		if err != nil {
			// No config.json - show help
			fmt.Println("No command line arguments and no config.json found.")
			fmt.Println("Either provide command line arguments or create a config.json file.")
			printHelp()
			os.Exit(1)
		}

		// Config exists - first apply the settings
		fmt.Println("Applying power settings from config.json")
		applyConfigSettings(cfg, count)

		// Check if we should start the API server
		if cfg.StartAPIServer {
			if cfg.APIKey == "" {
				fmt.Println("Error: API key is required to start API server")
				fmt.Println("Please add 'apiKey' field to your config.json or set 'startAPIServer' to false")
				os.Exit(1)
			}

			// API server mode
			config = cfg // Set global config
			err = initNVML()
			if err != nil {
				log.Fatalf("Failed to initialize NVML cache: %v", err)
			}

			fmt.Println("Starting API server mode")
			startAPIServer()
		} else {
			fmt.Println("Applied settings from config.json, exiting")
		}
	}
}
