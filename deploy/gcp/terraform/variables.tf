variable "project_id" {
  description = "GCP project that hosts edgecache."
  type        = string
}

variable "region" {
  description = "Region for Cloud Run and Artifact Registry. Tier 1 pricing regions (us-central1, us-east1, europe-west1, ...) are cheapest."
  type        = string
  default     = "us-central1"
}

variable "service_name" {
  description = "Cloud Run service name; also names the service accounts, the secret and the Artifact Registry repo."
  type        = string
  default     = "edgecache"
}

variable "origin_url" {
  description = "EDGECACHE_ORIGIN: the upstream base URL the proxy fronts (http or https, no query or fragment)."
  type        = string

  validation {
    condition     = can(regex("^https?://[^/?#]+[^?#]*$", var.origin_url))
    error_message = "The origin_url value must be an http:// or https:// URL with a host and no query or fragment."
  }
}

variable "image" {
  description = "Initial container image. CI ships new revisions with `gcloud run deploy --image`, so Terraform ignores later image changes. The default is Google's public hello container so the first apply works before anything is pushed."
  type        = string
  default     = "us-docker.pkg.dev/cloudrun/container/hello"
}

variable "min_instances" {
  description = "Instances kept warm. 0 means every scale-from-zero starts with an empty cache; 1 keeps one warm cache and bills for it all month (see README)."
  type        = number
  default     = 1

  validation {
    condition     = var.min_instances >= 0 && var.min_instances <= 10
    error_message = "The min_instances value must be in [0, 10]."
  }
}

variable "max_instances" {
  description = "Upper bound on instances. Every instance holds its own cache, so more instances means a lower hit ratio and more origin traffic; keep this small."
  type        = number
  default     = 4

  validation {
    condition     = var.max_instances >= 1 && var.max_instances <= 100
    error_message = "The max_instances value must be in [1, 100]."
  }
}

variable "concurrency" {
  description = "max_instance_request_concurrency. A hit is ~7 us of handler CPU and a miss mostly waits on the origin, so one vCPU carries far more than Cloud Run's default of 80 requests in flight."
  type        = number
  default     = 250

  validation {
    condition     = var.concurrency >= 1 && var.concurrency <= 1000
    error_message = "Cloud Run allows 1..1000 concurrent requests per instance."
  }
}

variable "cpu" {
  description = "vCPU per instance (Cloud Run limit string)."
  type        = string
  default     = "1"
}

variable "memory" {
  description = "Memory limit per instance. Holds the Go heap plus EDGECACHE_MEM_BYTES; keep cache_mem_bytes at about half of it."
  type        = string
  default     = "1Gi"
}

variable "cache_mem_bytes" {
  description = "EDGECACHE_MEM_BYTES: in-memory tier budget per instance."
  type        = string
  default     = "512MiB"
}

variable "go_mem_limit" {
  description = "GOMEMLIMIT: soft Go heap limit just under the container limit, so the GC tightens before Cloud Run kills the instance for exceeding memory."
  type        = string
  default     = "900MiB"
}

variable "ingress" {
  description = "INGRESS_TRAFFIC_ALL for a directly public cache, INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER when an external HTTPS load balancer (optionally with Cloud CDN / Cloud Armor) sits in front, INGRESS_TRAFFIC_INTERNAL_ONLY for VPC-internal callers."
  type        = string
  default     = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"

  validation {
    condition     = contains(["INGRESS_TRAFFIC_ALL", "INGRESS_TRAFFIC_INTERNAL_ONLY", "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"], var.ingress)
    error_message = "The ingress value must be INGRESS_TRAFFIC_ALL, INGRESS_TRAFFIC_INTERNAL_ONLY or INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER."
  }
}

variable "allow_unauthenticated" {
  description = "Grant roles/run.invoker to allUsers. A public HTTP cache needs it; it stays false until the ingress path is decided."
  type        = bool
  default     = false
}

variable "github_repository" {
  description = "owner/repo whose GitHub Actions OIDC tokens may deploy through Workload Identity Federation."
  type        = string
  default     = "crowbett/edgecache"
}

variable "github_deploy_ref" {
  description = "Only OIDC tokens minted for this git ref may impersonate the deployer service account."
  type        = string
  default     = "refs/heads/main"
}

variable "alert_email" {
  description = "Email for the alert notification channel; empty creates the policies with no channel (they still show in the console)."
  type        = string
  default     = ""
}

variable "alert_5xx_ratio" {
  description = "Fire when 5xx responses exceed this fraction of all requests."
  type        = number
  default     = 0.05
}

variable "alert_p99_latency_ms" {
  description = "Fire when the p99 request latency measured by Cloud Run exceeds this many milliseconds."
  type        = number
  default     = 500
}
