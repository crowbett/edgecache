# Alerts built on Cloud Run's own request metrics (run.googleapis.com/*), so
# they work without scraping edgecache's /metrics. Those metrics are measured
# at the Cloud Run front end: they include origin 5xx that the proxy passes
# through, and the latency of every MISS (origin round trip included).

locals {
  run_filter = join(" AND ", [
    "resource.type = \"cloud_run_revision\"",
    "resource.labels.service_name = \"${var.service_name}\"",
  ])
  channels = var.alert_email == "" ? [] : [google_monitoring_notification_channel.email[0].name]
}

resource "google_monitoring_notification_channel" "email" {
  count = var.alert_email == "" ? 0 : 1

  display_name = "edgecache on-call email"
  type         = "email"
  labels = {
    email_address = var.alert_email
  }

  depends_on = [google_project_service.apis]
}

resource "google_monitoring_alert_policy" "error_ratio" {
  display_name          = "${var.service_name}: 5xx ratio > ${var.alert_5xx_ratio * 100}%"
  combiner              = "OR"
  notification_channels = local.channels

  conditions {
    display_name = "5xx / all requests over 5 min"

    condition_threshold {
      filter = join(" AND ", [
        local.run_filter,
        "metric.type = \"run.googleapis.com/request_count\"",
        "metric.labels.response_code_class = \"5xx\"",
      ])
      denominator_filter = join(" AND ", [
        local.run_filter,
        "metric.type = \"run.googleapis.com/request_count\"",
      ])

      aggregations {
        alignment_period     = "300s"
        per_series_aligner   = "ALIGN_DELTA"
        cross_series_reducer = "REDUCE_SUM"
      }
      denominator_aggregations {
        alignment_period     = "300s"
        per_series_aligner   = "ALIGN_DELTA"
        cross_series_reducer = "REDUCE_SUM"
      }

      comparison      = "COMPARISON_GT"
      threshold_value = var.alert_5xx_ratio
      duration        = "300s"
      trigger {
        count = 1
      }
    }
  }

  alert_strategy {
    auto_close = "1800s"
  }

  documentation {
    mime_type = "text/markdown"
    content   = <<-EOT
      More than ${var.alert_5xx_ratio * 100}% of requests to Cloud Run service `${var.service_name}` returned 5xx for 5 minutes.
      Cloud Run counts origin 5xx that edgecache passes through, so check the origin first:
      `edgecache_origin_requests_total{status_class="5xx"}` vs `edgecache_responses_total{status_class="5xx"}` in the logs/metrics,
      then the revision logs (`resource.type="cloud_run_revision"`). stale-if-error on the origin's responses
      (or `EDGECACHE_DEFAULT_SIE`) lets the cache answer through an origin outage.
    EOT
  }
}

resource "google_monitoring_alert_policy" "latency_p99" {
  display_name          = "${var.service_name}: p99 latency > ${var.alert_p99_latency_ms} ms"
  combiner              = "OR"
  notification_channels = local.channels

  conditions {
    display_name = "p99 request latency over 5 min"

    condition_threshold {
      filter = join(" AND ", [
        local.run_filter,
        "metric.type = \"run.googleapis.com/request_latencies\"",
      ])

      aggregations {
        alignment_period     = "300s"
        per_series_aligner   = "ALIGN_PERCENTILE_99"
        cross_series_reducer = "REDUCE_MAX"
      }

      comparison      = "COMPARISON_GT"
      threshold_value = var.alert_p99_latency_ms
      duration        = "300s"
      trigger {
        count = 1
      }
    }
  }

  alert_strategy {
    auto_close = "1800s"
  }

  documentation {
    mime_type = "text/markdown"
    content   = <<-EOT
      p99 latency of `${var.service_name}` is above ${var.alert_p99_latency_ms} ms for 5 minutes. A HIT is sub-millisecond,
      so a high p99 means misses: a cold cache after scale-out or a restart (check instance count and
      `edgecache_requests_total{outcome="miss"}`), a slow origin, or CPU saturation at the configured concurrency.
    EOT
  }
}
