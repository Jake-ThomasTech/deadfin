{{- define "deadfin-worker.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "deadfin-worker.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "deadfin-worker.labels" -}}
app.kubernetes.io/name: {{ include "deadfin-worker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "deadfin-worker.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}

{{- define "deadfin-worker.tokenSecret" -}}
{{- if .Values.token.existingSecret -}}
{{ .Values.token.existingSecret }}
{{- else -}}
{{ include "deadfin-worker.fullname" . }}-token
{{- end -}}
{{- end -}}

{{/*
deadfin-worker.pod renders a worker pod spec. The context is a dict with
root (the chart root), worker (the merged worker settings), pools (the
proposed pool labels), ephemeral (bool), and scheduling (nodeSelector,
tolerations, affinity).
*/}}
{{- define "deadfin-worker.pod" -}}
{{- $root := .root -}}
{{- $w := .worker -}}
{{- $values := $root.Values -}}
{{- $hardwareDevice := and $values.gpu.mountDri (has $w.hwType (list "qsv" "vaapi")) -}}
{{- with $values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $values.gpu.runtimeClassName }}
runtimeClassName: {{ . }}
{{- end }}
terminationGracePeriodSeconds: {{ $values.terminationGracePeriodSeconds }}
securityContext:
  {{- toYaml $values.podSecurityContext | nindent 2 }}
  {{- with $values.gpu.supplementalGroups }}
  supplementalGroups:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- with .scheduling.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .scheduling.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .scheduling.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- if not .ephemeral }}
# A hostPath directory that the kubelet creates belongs to root: give it to
# the worker user, which writes the identity there.
initContainers:
  - name: state-owner
    image: {{ include "deadfin-worker.image" $root }}
    imagePullPolicy: {{ $values.image.pullPolicy }}
    command: [chown, "{{ $values.podSecurityContext.runAsUser }}:{{ $values.podSecurityContext.runAsGroup }}", /state]
    securityContext:
      runAsUser: 0
      runAsGroup: 0
      runAsNonRoot: false
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: [ALL]
        add: [CHOWN]
    resources:
      requests: { cpu: 1m, memory: 8Mi }
    volumeMounts:
      - name: state
        mountPath: /state
{{- end }}
containers:
  - name: worker
    image: {{ include "deadfin-worker.image" $root }}
    imagePullPolicy: {{ $values.image.pullPolicy }}
    args:
      - worker
      - -server={{ required "server.url is required" $values.server.url }}
      - -name=$(DEADFIN_WORKER_NAME)
      - -work-dir=/work
      - -max-jobs={{ $w.maxJobs }}
      - -input-mode={{ $w.inputMode }}
      - -drain-grace={{ $w.drainGrace }}
      - -max-held-segments={{ $w.maxHeldSegments }}
      {{- if .ephemeral }}
      - -ephemeral
      {{- else }}
      - -state-dir=/state
      {{- end }}
      {{- with .pools }}
      - -pool={{ join "," . }}
      {{- end }}
      {{- with $w.hwType }}
      - -hw-type={{ . }}
      {{- if has . (list "qsv" "vaapi") }}
      - -render-node={{ $w.renderNode }}
      {{- end }}
      {{- end }}
      {{- with $w.hwDecode }}
      - -hw-decode={{ join "," . }}
      {{- end }}
      {{- if $w.hwSessions }}
      - -hw-sessions={{ $w.hwSessions }}
      {{- end }}
      {{- range $values.worker.pathMaps }}
      - -path-map={{ . }}
      {{- end }}
      {{- if $values.server.insecure }}
      - -insecure
      {{- end }}
      {{- range $values.worker.extraArgs }}
      - {{ . | quote }}
      {{- end }}
    env:
      - name: DEADFIN_WORKER_NAME
        valueFrom:
          fieldRef:
            fieldPath: {{ if .ephemeral }}metadata.name{{ else }}spec.nodeName{{ end }}
      - name: DEADFIN_WORKER_TOKEN
        valueFrom:
          secretKeyRef:
            name: {{ include "deadfin-worker.tokenSecret" $root }}
            key: {{ $values.token.key }}
      {{- with $values.worker.env }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    securityContext:
      {{- toYaml $values.securityContext | nindent 6 }}
    {{- with $w.resources }}
    resources:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    volumeMounts:
      - name: work
        mountPath: /work
      - name: tmp
        mountPath: /tmp
      {{- if not .ephemeral }}
      - name: state
        mountPath: /state
      {{- end }}
      {{- if $values.media.enabled }}
      - name: media
        mountPath: {{ $values.media.mountPath }}
        readOnly: true
      {{- end }}
      {{- if $hardwareDevice }}
      - name: dri
        mountPath: /dev/dri
      {{- end }}
volumes:
  - name: work
    emptyDir: {}
  - name: tmp
    emptyDir: {}
  {{- if not .ephemeral }}
  - name: state
    hostPath:
      path: {{ $values.daemonset.stateHostPath }}
      type: DirectoryOrCreate
  {{- end }}
  {{- if $values.media.enabled }}
  - name: media
    {{- required "media.volume is required when media.enabled" $values.media.volume | toYaml | nindent 4 }}
  {{- end }}
  {{- if $hardwareDevice }}
  - name: dri
    hostPath:
      path: /dev/dri
      type: Directory
  {{- end }}
{{- end -}}
