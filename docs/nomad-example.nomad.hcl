job "mc-router" {
  datacenters = ["dc1"]
  type = "service"

  update {
    max_parallel = 1
    min_healthy_time = "10s"
    healthy_deadline = "5m"
    progress_deadline = "10m"
    auto_revert = false
    canary = 0
  }

  migrate {
    max_parallel = 1
    health_check = "checks"
    min_healthy_time = "10s"
    healthy_deadline = "5m"
  }

  group "mc-router" {
    count = 1

    network {
      port "minecraft" {
        static = 25565
      }
    }

    service {
      name     = "mc-router"
      tags     = ["global", "minecraft", "tcp"]
      port     = "minecraft"
      provider = "consul"

      check {
        name     = "alive"
        type     = "tcp"
        port     = "minecraft"
        interval = "10s"
        timeout  = "2s"
      }
    }

    restart {
      attempts = 2
      interval = "30m"
      delay = "15s"
      mode = "delay"
    }

    task "mc-router" {
      driver = "docker"

      config {
        image = "itzg/mc-router"
        ports = ["minecraft"]
        auth_soft_fail = true
      }

      resources {
        cpu        = 2000  # 2000Mhz
        memory     = 256 # 256MB
      }

      template {
        data = <<EOF
{
  "mappings": {
    {{- $first := true -}}
    {{- range service "mc-router-register.minecraft" }}
      {{- if not $first }},{{ end }}
      "{{ .ServiceMeta.externalServerName }}": "{{ .Address }}:{{ .Port }}"
      {{- $first = false }}
    {{- end }}
  }
}
EOF
        destination = "local/routes.json"
        change_mode = "signal"
        change_signal = "SIGHUP"
      }

      env {
        ROUTES_CONFIG = "local/routes.json"
        DEBUG = "true"
        TRACE = "true"
      }
    }
  }
}
