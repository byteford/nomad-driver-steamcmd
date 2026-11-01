job "foundry-proton" {
  datacenters = ["dc1"]

  group "foundry" {
    network {
      port "game" {
        static = 3724
      }
    }

    # Install, Proton prefix and saves live on one volume so they survive
    # reschedules. Both tasks mount it at <task dir>/data.
    volume "data" {
      type            = "csi"
      source          = "foundry"
      access_mode     = "single-node-writer"
      attachment_mode = "file-system"
    }

    # 1. Download/update the game. No `launch` block == install-only.
    #    (Prestart tasks run in parallel with each other, so keep this the
    #    only prestart task that writes install_dir.)
    task "install" {
      driver = "steamcmd"

      lifecycle {
        hook    = "prestart"
        sidecar = false
      }

      volume_mount {
        volume      = "data"
        destination = "data"
      }

      config {
        app_id          = "2915550"
        platform        = "windows"
        install_dir     = "data/steamapp"
        install_timeout = "30m"
      }

      resources {
        cpu    = 1000
        memory = 1024
      }
    }

    # 2. Run the server only. Proton builds its prefix on first run.
    task "server" {
      driver = "steamcmd"

      volume_mount {
        volume      = "data"
        destination = "data"
      }

      config {
        app_id           = "2915550"
        platform         = "windows"
        install_dir      = "data/steamapp"
        proton_path      = "/opt/GE-Proton9-20"
        compat_data_path = "data/compat"
        windows_display  = true # Unity app; still needs a display under Proton

        launch {
          command = "FoundryDedicatedServer.exe"
          args    = ["-batchmode", "-nographics"]
        }
      }

      resources {
        cpu    = 4000
        memory = 4096
      }

      kill_timeout = "60s"
    }
  }
}
