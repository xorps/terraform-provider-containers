provider "containers" {
  registry {
    hostname = "ghcr.io"
    username = var.ghcr_username
    password = var.ghcr_password
  }

  registry {
    hostname = "docker.io"
    username = var.dockerhub_username
    password = var.dockerhub_password
  }
}
