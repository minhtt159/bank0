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

{{/* api pod template — shared by the Deployment and the Rollout (rollout.enabled
     renders one OR the other, never both) so they can't drift: a probe/env fix
     lands in whichever kind a release runs.
     Call with root context; emits at column 0, callers nindent. */}}
{{- define "bank0.apiPodTemplate" -}}
metadata:
  labels:
    app.kubernetes.io/name: bank0
    app.kubernetes.io/instance: {{ .Release.Name }}
    app.kubernetes.io/component: api
spec:
  automountServiceAccountToken: false
  securityContext:
    {{- toYaml .Values.podSecurityContext | nindent 4 }}
  {{- if .Values.image.pullSecrets }}
  {{- include "bank0.imagePullSecrets" . | nindent 2 }}
  {{- end }}
  containers:
    - name: app
      image: {{ include "bank0.image" . }}
      imagePullPolicy: {{ .Values.image.pullPolicy }}
      args: ["serve"]
      ports:
        - { name: http, containerPort: 8080 }
      env:
        - { name: APP_SERVER_MODE, value: "api" }
        - { name: APP_ADMIN_RUN_MAINTENANCE, value: {{ .Values.api.runMaintenance | quote }} }
        - { name: APP_ADMIN_SESSION_IDLE_TIMEOUT, value: {{ .Values.admin.sessionIdleTimeout | quote }} }
        - { name: APP_SERVER_TRUST_PROXY_HEADERS, value: {{ .Values.trustProxyHeaders | quote }} }
        - { name: APP_SERVER_TRUSTED_PROXY_HOPS, value: {{ .Values.trustedProxyHops | quote }} }
        - { name: APP_SERVER_AUTO_MIGRATE, value: "false" }
        - { name: APP_LOGGING_ENCODING, value: {{ .Values.logging.encoding | quote }} }
        - { name: APP_LOGGING_LEVEL, value: {{ .Values.logging.level | quote }} }
        - { name: APP_APP_ENV, value: "production" }
        {{- include "bank0.dsnEnv" . | nindent 8 }}
        {{- include "bank0.jwtEnv" . | nindent 8 }}
      readinessProbe:
        # DB-aware: /readyz pings Postgres. Dead/exhausted pool = pulled from
        # rotation instead of serving 500s.
        httpGet: { path: /readyz, port: http }
        initialDelaySeconds: 3
        periodSeconds: 10
      livenessProbe:
        # Cheap, DB-blind: a DB blip must not kill the pod.
        httpGet: { path: /health, port: http }
        initialDelaySeconds: 5
        periodSeconds: 15
      resources:
        {{- toYaml .Values.api.resources | nindent 8 }}
      securityContext:
        {{- toYaml .Values.securityContext | nindent 8 }}
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
