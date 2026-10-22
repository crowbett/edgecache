terraform {
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.5"
    }
  }

  # Partial config: pass the bucket at init time, e.g.
  #   terraform init -backend-config="bucket=<project>-tfstate" -backend-config="prefix=edgecache"
  # Validated with `terraform init -backend=false`; never initialised against a real bucket.
  backend "gcs" {}
}

provider "google" {
  project = var.project_id
  region  = var.region
}
