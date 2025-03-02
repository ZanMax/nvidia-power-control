# Nvidia Power Control
Nvidia GPUs Power Limits Control

## Requirements
- NVIDIA GPUs with NVML support
- NVIDIA drivers 520 or higher

## Build
```bash
go build -o nvidia_power_control
```

## Installation
```bash
sudo cp nvidia_power_control /usr/local/bin/
```

## Configuration
```json
{
  "mode": "manual",
  "powerLimit": 380,
  "manualLimits": {
    "0": 380,
    "1": 370,
    "2": 360,
    "3": 350,
    "4": 340,
    "5": 330
  },
  "startAPIServer": true,
  "apiKey": "<api-key>",
  "apiPort": 8080
}
```

## Service
```bash
sudo nano /etc/systemd/system/nvidia_power_control.service
```

## Check Logs
```bash
journalctl -u nvidia_power_control.service -f
```