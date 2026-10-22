locals {
  apis = [
    "run.googleapis.com",
    "artifactregistry.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "sts.googleapis.com",
    "secretmanager.googleapis.com",
    "monitoring.googleapis.com",
    # "vpcaccess.googleapis.com", # only with the Serverless VPC Access connector below
  ]
}

resource "google_project_service" "apis" {
  for_each = toset(local.apis)

  service            = each.value
  disable_on_destroy = false
}

# ---------- Artifact Registry ----------

resource "google_artifact_registry_repository" "edgecache" {
  location      = var.region
  repository_id = var.service_name
  format        = "DOCKER"
  description   = "edgecache container images (pushed by .github/workflows/deploy-gcp.yml)"

  # Keep the 10 newest versions and every v* release tag; delete the rest
  # after 30 days so storage stays within the free 0.5 GB.
  cleanup_policy_dry_run = false
  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions {
      keep_count = 10
    }
  }
  cleanup_policies {
    id     = "keep-releases"
    action = "KEEP"
    condition {
      tag_state    = "TAGGED"
      tag_prefixes = ["v"]
    }
  }
  cleanup_policies {
    id     = "delete-old"
    action = "DELETE"
    condition {
      older_than = "2592000s" # 30 days
    }
  }

  depends_on = [google_project_service.apis]
}

# ---------- Runtime identity ----------

# The service runs as its own account with no project-level roles. It needs
# exactly one permission: read the purge-token secret. Image pulls from a
# same-project Artifact Registry repo are done by the Cloud Run service agent,
# and stdout logs plus the built-in request metrics need no grant.
resource "google_service_account" "runtime" {
  account_id   = "${var.service_name}-run"
  display_name = "edgecache Cloud Run runtime"
}

# EDGECACHE_PURGE_TOKEN guards /admin/purge once the admin listener binds off
# loopback (config.Validate refuses to start without it). Only the secret
# container is managed here; the value is added out of band so it never lands
# in Terraform state:
#   openssl rand -hex 32 | gcloud secrets versions add edgecache-purge-token --data-file=-
resource "google_secret_manager_secret" "purge_token" {
  secret_id = "${var.service_name}-purge-token"

  replication {
    auto {}
  }

  depends_on = [google_project_service.apis]
}

resource "google_secret_manager_secret_iam_member" "runtime_reads_purge_token" {
  secret_id = google_secret_manager_secret.purge_token.id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.runtime.member
}

# ---------- Optional: Serverless VPC Access connector ----------
# Only needed when the origin is on a private VPC address. Direct VPC egress
# (the commented vpc_access block in cloudrun.tf) is usually the better choice
# now: a connector bills its own e2-micro instances around the clock.
#
# resource "google_vpc_access_connector" "edgecache" {
#   name          = "${var.service_name}-conn"
#   region        = var.region
#   network       = "default"
#   ip_cidr_range = "10.8.0.0/28"
#   min_instances = 2
#   max_instances = 3
#   machine_type  = "e2-micro"
# }
