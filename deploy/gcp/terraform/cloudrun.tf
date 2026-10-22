locals {
  proxy_port = 8080 # the Cloud Run ingress port: everything on it is proxied to the origin
  admin_port = 9101 # health, metrics, purge; reachable only from inside the instance (probes)
}

resource "google_cloud_run_v2_service" "edgecache" {
  name     = var.service_name
  location = var.region
  ingress  = var.ingress

  # A cache is disposable, but deleting the service by accident still drops
  # traffic; flip this off deliberately before a destroy.
  deletion_protection = true

  template {
    service_account                  = google_service_account.runtime.email
    execution_environment            = "EXECUTION_ENVIRONMENT_GEN2"
    max_instance_request_concurrency = var.concurrency
    # Matches EDGECACHE_ORIGIN_TIMEOUT (60s): a miss can legitimately take that long.
    timeout = "60s"

    scaling {
      min_instance_count = var.min_instances
      max_instance_count = var.max_instances
    }

    containers {
      name  = "edgecache"
      image = var.image

      ports {
        name           = "http1"
        container_port = local.proxy_port
      }

      resources {
        limits = {
          cpu    = var.cpu
          memory = var.memory
        }
        # CPU always allocated (instance-based billing). edgecache does real work
        # outside the request that triggered it: the collapse leader keeps
        # fetching after its client disconnects (orphan fetch), stale-while-
        # revalidate and range-fill refreshes run after the response is sent,
        # and the janitor ticks every 30s to sweep tombstones, reconcile the tag
        # index and refresh gauges. With cpu_idle = true Cloud Run throttles the
        # CPU to near zero between requests, so all of that stalls until the next
        # request arrives. See deploy/gcp/README.md for the cost of this choice.
        cpu_idle          = false
        startup_cpu_boost = true
      }

      env {
        name  = "EDGECACHE_ORIGIN"
        value = var.origin_url
      }
      env {
        name  = "EDGECACHE_ADDR"
        value = "0.0.0.0:${local.proxy_port}"
      }
      env {
        # Off loopback so the platform's probes can reach /healthz and /readyz.
        # Cloud Run only routes external traffic to the ingress port, so this
        # listener is still not reachable from outside the instance; the purge
        # token below is required by config.Validate anyway.
        name  = "EDGECACHE_ADMIN_ADDR"
        value = "0.0.0.0:${local.admin_port}"
      }
      env {
        name  = "EDGECACHE_MEM_BYTES"
        value = var.cache_mem_bytes
      }
      env {
        # The disk tier stays off: Cloud Run's writable filesystem is in-memory
        # and counts against the same memory limit, so a "disk" tier there would
        # just be a slower second copy in RAM.
        name  = "EDGECACHE_DISK_DIR"
        value = ""
      }
      env {
        name  = "GOMEMLIMIT"
        value = var.go_mem_limit
      }
      env {
        # Cloud Run sends SIGTERM and allows 10s before SIGKILL; the staged
        # teardown's proxy drain (stage 2) must finish inside that.
        name  = "SHUTDOWN_TIMEOUT"
        value = "8s"
      }
      env {
        name  = "APP_ENV"
        value = "prod"
      }
      env {
        name  = "LOG_FORMAT"
        value = "json"
      }
      env {
        name = "EDGECACHE_PURGE_TOKEN"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.purge_token.secret_id
            version = "latest"
          }
        }
      }

      # Readiness flips on once the store is open and both listeners are bound;
      # poll it fast so a cold start is admitted as soon as it can serve.
      startup_probe {
        http_get {
          path = "/readyz"
          port = local.admin_port
        }
        initial_delay_seconds = 0
        period_seconds        = 1
        timeout_seconds       = 1
        failure_threshold     = 30
      }

      # /healthz is process liveness only; it does not touch the origin, so an
      # origin outage never makes Cloud Run restart (and empty) a healthy cache.
      liveness_probe {
        http_get {
          path = "/healthz"
          port = local.admin_port
        }
        period_seconds    = 15
        timeout_seconds   = 2
        failure_threshold = 3
      }
    }

    # Private origin? Prefer Direct VPC egress over a connector:
    # vpc_access {
    #   network_interfaces {
    #     network    = "default"
    #     subnetwork = "default"
    #   }
    #   egress = "PRIVATE_RANGES_ONLY"
    # }
    # or, with the connector in main.tf:
    # vpc_access {
    #   connector = google_vpc_access_connector.edgecache.id
    #   egress    = "PRIVATE_RANGES_ONLY"
    # }
  }

  traffic {
    type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
    percent = 100
  }

  lifecycle {
    # CI owns the image (gcloud run deploy --image ...); Terraform owns the shape.
    ignore_changes = [
      template[0].containers[0].image,
      client,
      client_version,
    ]
  }

  depends_on = [
    google_project_service.apis,
    google_secret_manager_secret_iam_member.runtime_reads_purge_token,
  ]
}

resource "google_cloud_run_v2_service_iam_member" "public" {
  count = var.allow_unauthenticated ? 1 : 0

  name     = google_cloud_run_v2_service.edgecache.name
  location = google_cloud_run_v2_service.edgecache.location
  role     = "roles/run.invoker"
  member   = "allUsers"
}
