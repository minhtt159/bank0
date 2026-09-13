{{- define "bank0.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{/* Pull secrets for a private GHCR package. Guard the call with an if — an
     empty list must render nothing at all. Call with root context. */}}
{{- define "bank0.imagePullSecrets" -}}
imagePullSecrets:
{{- range .Values.image.pullSecrets }}
  - name: {{ . }}
{{- end }}
{{- end -}}

{{- define "bank0.dsnSecretName" -}}
{{- if .Values.database.existingSecret }}{{ .Values.database.existingSecret }}{{- else }}{{ .Release.Name }}-db{{- end -}}
{{- end -}}

{{/* DSN env, sourced from the secret. Call with root context. */}}
{{- define "bank0.dsnEnv" -}}
- name: APP_DATABASE_DSN
  valueFrom:
    secretKeyRef:
      name: {{ include "bank0.dsnSecretName" . }}
      key: {{ .Values.database.secretKey }}
{{- end -}}

{{- define "bank0.authSecretName" -}}
{{- if .Values.auth.existingSecret }}{{ .Values.auth.existingSecret }}{{- else }}{{ .Release.Name }}-auth{{- end -}}
{{- end -}}

{{/* JWT secret env for the client surface, if configured. Call with root context. */}}
{{- define "bank0.jwtEnv" -}}
{{- if or .Values.auth.existingSecret .Values.auth.jwtSecret }}
- name: APP_AUTH_JWT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ include "bank0.authSecretName" . }}
      key: {{ .Values.auth.secretKey }}
{{- end }}
{{- end -}}

{{/*
  parentRef for one surface's HTTPRoute.

  Both surfaces attach to the chart's `gateway` by default. A surface can override
  any of name/namespace/sectionName via `<surface>.parentRef` — which is what
  exposing the api to the internet needs: the api route re-parented to the
  platform's external Gateway (cloudflared in front of it) while the portal stays
  on the internal one. See docs/04-deployment.md §3, "Exposing the client API".

  Call with a dict: (dict "root" $ "surface" .Values.api "listener" "https-api")
*/}}
{{- define "bank0.parentRef" -}}
{{- $gw := .root.Values.gateway -}}
{{- $ref := .surface.parentRef | default dict -}}
- name: {{ $ref.name | default $gw.name }}
  namespace: {{ $ref.namespace | default $gw.namespace | default .root.Release.Namespace }}
  sectionName: {{ $ref.sectionName | default (ternary .listener "http" $gw.tls.enabled) }}
{{- end -}}

{{/* True when a surface attaches to a Gateway other than the chart's own. */}}
{{- define "bank0.hasForeignParent" -}}
{{- $ref := .parentRef | default dict -}}
{{- if or $ref.name $ref.namespace }}true{{ end }}
{{- end -}}
