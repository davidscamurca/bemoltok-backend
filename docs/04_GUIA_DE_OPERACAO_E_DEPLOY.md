# Guia de Operação, Configuração e Deploy

Este documento é o manual prático para desenvolvedores e operadores do **BemolTok**, contendo a lista completa de variáveis de ambiente, segredos, comandos de build e procedimentos de deploy para o backend (Cloud Run) e frontend móvel (Flutter / Android).

---

## 1. Topologia de Infraestrutura (Google Cloud Platform)

- **Projeto GCP**: `bemoltok-dev` (Número do projeto: `119999976463`)
- **Região Principal**: `southamerica-east1` (São Paulo, Brasil)
- **Serviço Cloud Run**: `bemoltok-bff`
  - URL Base: `https://bemoltok-bff-bium6anima-rj.a.run.app`
  - Recursos recomendados: 4 vCPU / 8 GiB RAM (vetores de embeddings carregados em memória).
  - Escala: Mínimo de 0 instâncias (escala para zero sob demanda) ou 1 instância para baixa latência de cold start.
- **Armazenamento de Mídia e Artefatos**:
  - Bucket UGC: `bemoltok-dev-ugc`
  - Bucket de Modelos ML: `bemoltok-dev-artifacts`

---

## 2. Variáveis de Ambiente e Segredos (Secret Manager)

### 2.1. Backend (Cloud Run)

| Variável / Secret | Origem | Obrigatória? | Descrição |
|---|---|---|---|
| `PORT` | Cloud Run (automática) | Sim | Porta HTTP do container (padrão `8080`). |
| `GOOGLE_CLOUD_PROJECT` | Env Var | Sim | ID do projeto GCP (`bemoltok-dev`). |
| `AUTH_REQUIRED` | Env Var | Sim | `"true"` exige Firebase ID Token em todas as rotas Bearer. |
| `ALLOWED_EMAIL_DOMAIN` | Env Var | Sim | Domínio restrito para acesso (`bemol.com.br`). |
| `VTEX_APP_KEY` | Secret Manager | Sim | Chave de aplicação VTEX (`vtex-app-key:latest`). |
| `VTEX_APP_TOKEN` | Secret Manager | Sim | Token de autenticação VTEX (`vtex-app-token:latest`). |
| `VTEX_HOST` | Env Var | Não | Host do e-commerce (default: `bemol.vtexcommercestable.com.br`). |
| `SENDGRID_API_KEY` | Secret Manager | Sim | Chave de envio da API SendGrid para e-mails de OTP. |
| `SENDGRID_FROM_EMAIL`| Env Var | Não | Remetente dos e-mails (default: `naoresponda@bemol.com.br`). |
| `SEND_LINK_APP_KEY` | Secret Manager | Sim | Chave de app necessária nos headers de pré-login (`X-App-Key`). |
| `DIRECTORY_HMAC_SECRET` | Secret Manager | Sim | Chave secreta usada para calcular HMAC de e-mails corporativos. |
| `ADMIN_TOKEN` | Secret Manager | Não | Token de autorização para o endpoint `/admin/reload`. |
| `UGC_BUCKET_NAME` | Env Var | Sim | Nome do bucket GCS para vídeos e fotos (`bemoltok-dev-ugc`). |
| `ARTIFACTS_DIR` | Env Var | Não | Caminho dos arquivos ML montados (default: `/data/artifacts`). |

---

### 2.2. Frontend (Flutter / Mobile)

Configurado via arquivo `.env` ou `--dart-define-from-file=secrets.json`:

| Variável | Exemplo | Descrição |
|---|---|---|
| `API_BASE_URL` | `https://bemoltok-bff-bium6anima-rj.a.run.app` | URL base do BFF na nuvem. |
| `CLIENT_VERSION` | `1.0.0` | Versão enviada no header `X-Client-Version`. |
| `SEND_LINK_APP_KEY` | `<chave-secreta>` | Mesma chave de app esperada pelo BFF em `/auth/*`. |

---

## 3. Comandos Oficiais de Deploy e Execução

### 3.1. Deploy do Backend (Cloud Run)

Execute a partir da raiz do repositório backend (`/Users/nelsonpinto/dev/BemolTok/bemoltok-backend`):

```bash
cd /Users/nelsonpinto/dev/BemolTok/bemoltok-backend

gcloud run deploy bemoltok-bff \
  --source . \
  --region southamerica-east1 \
  --project bemoltok-dev \
  --update-env-vars AUTH_REQUIRED=true,ALLOWED_EMAIL_DOMAIN=bemol.com.br,GOOGLE_CLOUD_PROJECT=bemoltok-dev \
  --update-secrets=VTEX_APP_KEY=vtex-app-key:latest,VTEX_APP_TOKEN=vtex-app-token:latest
```

> **Atenção**: O comando acima faz build via Cloud Build na nuvem e sobe a nova revisão de forma atômica, migrando 100% do tráfego sem derrubar instâncias em andamento.

---

### 3.2. Execução do App Móvel em Modo Debug / Desenvolvimento

Para executar diretamente no smartphone físico conectado via cabo USB (ex: `moto g35 5G` - `ZF525HRQ43`):

```bash
cd /Users/nelsonpinto/dev/BemolTok/bemoltok-front

# 1. Identificar o ID do aparelho conectado
flutter devices

# 2. Executar com as variáveis de ambiente e segredos
flutter run --dart-define-from-file=secrets.json -d ZF525HRQ43
```

---

### 3.3. Compilação e Atualização de Release no Aparelho (Sem Apagar Pastas)

Quando for gerar uma versão final de release e atualizar o celular físico **sem desinstalar o app** (preservando o ícone organizado nas pastas da tela inicial do celular):

```bash
cd /Users/nelsonpinto/dev/BemolTok/bemoltok-front

# 1. Compilar o APK de release otimizado
flutter build apk --release --dart-define-from-file=secrets.json

# 2. Instalar diretamente via ADB substituindo o binário (-r = reinstall, preserva dados e pastas)
adb -s ZF525HRQ43 install -r build/app/outputs/flutter-apk/app-release.apk
```

---

## 4. Diagnóstico, Logs e Manutenção

### 4.1. Visualizar Logs em Tempo Real do Backend
Para inspecionar requisições, erros de autenticação ou chamadas da VTEX no Cloud Run:

```bash
gcloud run services logs tail bemoltok-bff \
  --project bemoltok-dev \
  --region southamerica-east1
```

### 4.2. Recarga Dinâmica dos Modelos de Recomendação (Hot-Swap)
Se novos embeddings forem exportados para o bucket de artefatos e você quiser atualizar a matriz em memória sem reiniciar o serviço:

```bash
curl -X POST https://bemoltok-bff-bium6anima-rj.a.run.app/admin/reload \
  -H "X-Admin-Token: SEU_ADMIN_TOKEN_AQUI"
```

### 4.3. Verificação de Saúde (Health Check)
Para verificar a conexão com o Firestore e a idade das estatísticas:

```bash
curl https://bemoltok-bff-bium6anima-rj.a.run.app/health
```
Resposta esperada:
```json
{"status":"ok","stats_age":"45s","firestore_connected":true}
```
