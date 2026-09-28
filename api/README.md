# bmltok-api — BFF de Recomendação (BemolTok)

Backend (BFF) em Go que serve o feed de recomendações do app BemolTok e expõe
identidade/perfil do usuário. Roda no **Google Cloud Run**, autentica via
**Firebase Auth** e persiste perfis no **Firestore**.

O motor de recomendação carrega embeddings (exportados pelo `bmltok/datascience/`)
e ranqueia em tempo real: `score = α·cosine(cliente, produto) + (1-α)·popularidade + γ·UCB1`.

> **Para o time de Frontend:** vá direto para a seção [Guia de integração Frontend](#guia-de-integração-frontend).

---

## Ambiente em produção (dev)

| Item | Valor |
|---|---|
| **Base URL** | `https://bemoltok-bff-119999976463.southamerica-east1.run.app` |
| Projeto GCP / Firebase | `bemoltok-dev` |
| Região | `southamerica-east1` (São Paulo) |
| Serviço Cloud Run | `bemoltok-bff` |
| Auth | Firebase Auth — **código por email (OTP)**, com custom token. Magic link mantido como alternativa |
| Envio de email | Branded via **SendGrid**, remetente `inteligenciadenegocios@bemol.com.br` |
| Domínio de email permitido | `@bemol.com.br` |
| Perfis de usuário | Firestore, coleção `users/{uid}` |
| Regras do Firestore | **lockdown** (`allow read, write: if false`) — acesso só pelo backend (Admin SDK) |

---

## Autenticação

O login é por **código de 6 dígitos no email (OTP)**: o app pede o código
(`POST /auth/request-code`), o usuário digita, o backend valida e devolve um
**custom token** (`POST /auth/verify-code`); o app troca por sessão via
`signInWithCustomToken`. Daí em diante envia o **ID Token** do Firebase em toda
requisição protegida:

```
Authorization: Bearer <Firebase ID Token>
```

- O backend **verifica** o token (Firebase Admin SDK) e extrai o `uid` e o `email`.
- Só são aceitos emails do domínio `@bemol.com.br` (senão `403`).
- Token ausente/ inválido em rota protegida → `401`.

> O **magic link** (`POST /auth/send-link`) continua disponível como alternativa,
> mas o fluxo recomendado é o OTP (entrega melhor no email corporativo e dispensa
> deep links/Universal Links no app).

### Identidade vs. ID_CLIENTE
Existem **duas identidades** e o backend liga uma à outra:

- `uid` — identidade do **Firebase** (criada no login).
- `ID_CLIENTE` — código do **cliente Bemol** (é o que tem embedding e gera o feed).

O backend tenta **vincular automaticamente** o `ID_CLIENTE` no primeiro `GET /me`:
consulta um diretório `email → ID_CLIENTE` (hasheado, ver abaixo) e, em caso de
acerto, grava o link com `client_id_verified: true`. Se não houver acerto, o app
cai no fluxo manual: o usuário informa o `ID_CLIENTE` (`POST /me/client-id`), que
fica gravado com `client_id_verified: false`. Em ambos os casos o link vai para
`users/{uid}.bemolClientId` e, a partir daí, o `GET /recommend` resolve
`uid → ID_CLIENTE → feed` sozinho.

### Envio do magic link (branded, via SendGrid)
Em vez do email padrão do Firebase, o app chama `POST /auth/send-link`: o backend
gera o link de sign-in (Firebase Admin) e envia um email com identidade visual
Bemol via SendGrid (remetente `inteligenciadenegocios@bemol.com.br`). Esse
endpoint é público (pré-login) e se protege sozinho — ver detalhes abaixo.

### Diretório email → ID_CLIENTE (auto-link)
- Artefato opcional `email_cli.hashed.csv` no diretório de artefatos, no formato
  `HMAC-SHA256(secret, email);ID_CLIENTE` (uma linha por usuário). **Não guarda
  email em texto puro.**
- Gerado offline pelo CLI em `api/tools/hashdir/` a partir de um CSV `email;id_cliente`.
- O backend carrega na subida (`DIRECTORY_FILE` / default `<artifacts>/email_cli.hashed.csv`)
  e hasheia o email do token com o mesmo segredo (`DIRECTORY_HMAC_SECRET`) para o lookup.
- Ausente → auto-link desligado (só fluxo manual).

### Rollout do token (concluído)
Variável `AUTH_REQUIRED` controla o legado:
- `true` (**estado atual em produção-dev**): rotas protegidas exigem **token**; o modo
  legado (`X-User-Id` / `GET /recommend/{id}`) está **desligado** (sem token → `401`).
- `false`: aceita token OU legado. Usar só se precisar reverter rapidamente
  (`--update-env-vars AUTH_REQUIRED=false`, sem rebuild).

---

## Endpoints

Legenda: 🔓 público · 🔑 requer token (Bearer) · 🔐 requer header de segredo.

> Todos os endpoints `POST` têm o corpo limitado a **1 MiB** (`413` se exceder).

### 🔓 `GET /health`
Mínimo de propósito (público): não expõe contagens de base.
```json
{ "status": "ok", "stats_age": "2m31s", "firestore_connected": true }
```

### 🔓 `POST /auth/send-link`
Gera o link de sign-in (Firebase) e envia o email branded via SendGrid.
```jsonc
// request body
{ "email": "fulano@bemol.com.br" }
```
- **Anti-abuso embutido**: valida o domínio `@bemol.com.br`, rate-limit por email
  (1/min) e por IP (10/h), e **sempre** responde `204` no caminho feliz (não revela
  se o email é cadastrado).
- **Chave de app (opcional, recomendada)**: quando `SEND_LINK_APP_KEY` está setada,
  o caller precisa enviar o header `X-App-Key: <segredo>` (compare constant-time);
  sem ele → `401`. O app injeta esse valor no build (`--dart-define`).
- Respostas: `204` (ok) · `400` (email inválido) · `401` (X-App-Key ausente/errado)
  · `403` (domínio não permitido) · `429` (rate-limit) · `502` (falha no SendGrid)
  · `503` (envio não configurado).

### 🔓 `POST /auth/request-code` · `POST /auth/verify-code` *(login por código/OTP)*
Alternativa ao magic link: código de 6 dígitos por email (entrega melhor no Outlook
corporativo e remove deep links/AASA do app). Mesma proteção do `/auth/send-link`
(domínio `@bemol.com.br`, rate-limit, `X-App-Key`).

`POST /auth/request-code` — gera o código, guarda o **HMAC** dele (TTL 10 min, máx
5 tentativas, uso único) em `auth_codes/{emailHMAC}` no Firestore e envia por SendGrid.
```jsonc
{ "email": "fulano@bemol.com.br" }   // sempre 204 (não revela cadastro)
```

`POST /auth/verify-code` — valida o código e, em caso de acerto, faz get-or-create
do usuário Firebase por email e devolve um **custom token**. O app troca por sessão
via `signInWithCustomToken` → mesmo ID Token de sempre.
```jsonc
// request
{ "email": "fulano@bemol.com.br", "code": "123456" }
// 200
{ "custom_token": "<firebase custom token>" }
```
- Erros: `400` (email/código inválido) · `401` (código errado/expirado, X-App-Key, ou >5 tentativas) · `403` (domínio) · `503` (OTP não configurado).
- Requer `roles/iam.serviceAccountTokenCreator` na SA do Cloud Run (assinatura do custom token).

### 🔐 `POST /admin/reload`
Hot-swap do índice de embeddings sem reiniciar o serviço (recarrega os artefatos).
- Exige o header `X-Admin-Token: <ADMIN_TOKEN>` (compare constant-time).
- Respostas: `200` (recarregado) · `401` (token errado) · `503` (desabilitado: `ADMIN_TOKEN` não setado) · `500` (falha ao carregar).

### 🔑 `GET /me`
Retorna (criando no primeiro acesso) o perfil do usuário autenticado.
```json
{
  "uid": "abc123",
  "email": "davidcamurca@bemol.com.br",
  "bemol_client_id": "",
  "client_id_verified": false,
  "created_at": "2026-06-02T15:00:00Z",
  "last_seen_at": "2026-06-02T15:00:00Z"
}
```
> Se `bemol_client_id` vier **vazio**, o app deve pedir o `ID_CLIENTE` ao usuário.

### 🔑 `POST /me/client-id`
Vincula (ou **edita**) o `ID_CLIENTE` do usuário. Sobrescreve o valor anterior.
```jsonc
// request body
{ "bemol_client_id": "131" }
```
- O id é validado contra o índice de embeddings. Se não existir → `422`.
- Resposta: o perfil atualizado (mesmo shape do `GET /me`).

### 🔑 `GET /recommend?k=10`
Feed do usuário autenticado (resolve `uid → ID_CLIENTE`).
- `409` se o usuário ainda não vinculou um `ID_CLIENTE`.
```json
{
  "client_id": "131",
  "recommendations": [
    { "rank": 1, "product_id": "373038", "name": "KIT MINI ANTENA STARLINK",
      "score": 0.388, "price": 906.6, "category": "114",
      "is_new": true, "propensity": 0.51 }
  ],
  "latency_ms": 10.3
}
```

### 🔑 `GET /recommend/{client_id}?k=10` *(legado)*
Mesma resposta acima, mas com o `client_id` no path. **Com `AUTH_REQUIRED=true`
(estado atual) exige token** — sem `Bearer` → `401`. Mantido apenas como rede de
segurança; será removido quando a flag `AUTH_REQUIRED` for aposentada.

### 🔓 `GET /similar/{product_id}?k=10`
Top-K produtos similares (vizinhos no espaço de embeddings). Não exige identidade.
```json
{ "product_id": "373038", "similar": [ ... ], "latency_ms": 3.8 }
```

### 🔑 `POST /events`
Ingestão de interações do app (batch). Identidade vem do **token** (não do body).
Persiste em Firestore (`events/{eventId}`) e atualiza os contadores que alimentam o UCB1.
```jsonc
{
  "events": [
    {
      "event_id": "uuid-v4",    // idempotência — opcional, mas recomendado
      "product_id": "373038",
      "category": "114",
      "kind": "like",          // view|dwell|like|unlike|comment|share|bookmark|click_out|skip
      "value": 1.0,
      "at": "2026-06-02T15:00:00Z",
      "rank": 0,                // posição no feed (p/ IPW) — opcional
      "propensity": 0.51        // P(item|policy) devolvido pelo /recommend — opcional
    }
  ]
}
```
- Header opcional: `X-Client-Version: <versao do app>`.
- Limites: **máx. 500 eventos por batch** (senão `400`) e corpo de até **1 MiB** (senão `413`).
- Resposta: `{ "inserted": N }` (duplicados pelo `event_id`/hash são ignorados → reenvio é seguro).

### 🔑 `POST /clickout`
Registra o sinal mais forte de conversão (usuário tocou pra comprar / abrir o produto).
Açúcar sobre um evento `click_out`.
```jsonc
// request body
{ "product_id": "373038", "category": "114", "rank": 0, "propensity": 0.51 }
```
- Resposta: `{ "status": "ok", "recorded": true }` (`recorded:false` = duplicado).

### 🔓 `GET /trends?k=10`
Top-K produtos em alta (por sinais positivos acumulados).
```json
{ "trends": [ { "rank": 1, "product_id": "373038",
  "name": "KIT MINI ANTENA STARLINK", "positive_signals": 3 } ] }
```

### 🔑 `GET /experiments`
Variantes de A/B test do usuário (bucketização determinística por `uid`).
Config lida da coleção Firestore `experiments/{id}`.
```json
{ "experiments": [ { "experiment_id": "reranker_v1", "variant": "B", "params": {} } ] }
```

### 🔑 `POST /posts` · `POST /posts/{id}/complete` · `GET /posts` *(UGC — fotos/vídeo)*
Usuário logado posta **3–5 fotos** *ou* **1 vídeo (≤ 2 min, da galeria)** amarrado a um
`product_id` (obrigatório; o app resolve a URL da loja na VTEX). A mídia **não passa
pelo Cloud Run**: vai direto pro bucket privado `bemoltok-dev-ugc` via **V4 signed URL**.

Fluxo em 3 passos:
1. `POST /posts` valida e cria o post (`awaiting_upload`), devolve **1 signed PUT URL por arquivo**.
2. O app faz `PUT` de cada arquivo na URL (com o `Content-Type` exato devolvido).
3. `POST /posts/{id}/complete` confere os objetos no bucket, aplica os caps e publica.

```jsonc
// 1) POST /posts  (Bearer) — abrir um post de 3 fotos
{ "product_id": "373038", "type": "photos",
  "items": [ {"content_type":"image/jpeg"}, {"content_type":"image/jpeg"}, {"content_type":"image/png"} ] }
// resposta 201:
{ "post_id": "uuid", "status": "awaiting_upload", "upload_expires_at": "2026-...Z",
  "uploads": [ { "object":"posts/<uid>/<post_id>/0.jpg", "content_type":"image/jpeg",
    "method":"PUT", "upload_url":"https://storage.googleapis.com/...assinada...",
    "headers": {"Content-Type":"image/jpeg"}, "max_bytes": 10485760 } ] }

// 2) (app) PUT upload_url  com header Content-Type igual e o binário no corpo

// 3) POST /posts/{post_id}/complete  (Bearer, sem corpo) → 200 com o post publicado
{ "id":"uuid", "product_id":"373038", "type":"photos", "status":"published",
  "media":[{"object":"posts/<uid>/<post_id>/0.jpg","content_type":"image/jpeg","size":812345}],
  "created_at":"...","published_at":"..." }
```
- **Tipos**: fotos `image/jpeg`|`image/png`; vídeo `video/mp4`|`video/quicktime`.
- **Caps**: foto ≤ 10 MiB; vídeo ≤ 100 MiB (rejeição com `413` no complete, com cleanup do objeto). Duração ≤ 120s é responsabilidade do app.
- **Quantidade**: `photos` = 3–5 itens; `video` = exatamente 1.
- `GET /posts` (Bearer) lista os posts **do próprio usuário** (publicados), com `media_urls` (signed GET URLs, validade 60 min) prontas para exibir.
- A URL de assinatura expira em **15 min** (upload) — abra o post pouco antes de subir.
- **Privacidade/moderação**: bucket privado (sem acesso público); publica **sem moderação** por ora — o campo `status` já está preparado para um futuro `pending_review` antes de entrar em qualquer feed.

---

## Guia de integração Frontend

Passo a passo do que o app **Flutter** precisa fazer:

**1. Configurar Firebase no app**
- Registrar o app Android/iOS no projeto `bemoltok-dev` (gera `google-services.json` / `GoogleService-Info.plist`).
- Adicionar o pacote `firebase_auth`.

**2. Login por código (OTP) — fluxo recomendado**

Dois passos, sem deep links/Universal Links: pede o código, valida e troca o
custom token por sessão Firebase.
```dart
// 2a. pedir o código (backend envia o email branded com 6 dígitos)
await http.post(
  Uri.parse('$apiBaseUrl/auth/request-code'),
  headers: {
    'Content-Type': 'application/json',
    if (appKey.isNotEmpty) 'X-App-Key': appKey, // mesmo segredo do send-link
  },
  body: jsonEncode({'email': 'davidcamurca@bemol.com.br'}),
); // 204 = enviado → mostrar tela "digite o código"

// 2b. validar o código → recebe um custom token
final res = await http.post(
  Uri.parse('$apiBaseUrl/auth/verify-code'),
  headers: {
    'Content-Type': 'application/json',
    if (appKey.isNotEmpty) 'X-App-Key': appKey,
  },
  body: jsonEncode({'email': email, 'code': code}),
); // 200 { "custom_token": "..." } | 401 código errado/expirado

// 2c. trocar por sessão Firebase
final customToken = (jsonDecode(res.body) as Map)['custom_token'] as String;
await FirebaseAuth.instance.signInWithCustomToken(customToken);
```
> A `SEND_LINK_APP_KEY` deve ser passada no build (`--dart-define-from-file=secrets.json`),
> inclusive nos builds de distribuição (`flutter build ipa/appbundle`), senão o
> backend responde `401`. Mantenha `secrets.json` fora do git.
>
> Alternativa (legado): magic link via `POST /auth/send-link` + `signInWithEmailLink`.
> O OTP dispensa deep links, Universal/App Links e a página `finishSignIn`.

**3. Pegar o ID Token e mandar em toda chamada protegida**
```dart
final idToken = await FirebaseAuth.instance.currentUser!.getIdToken();
// header em TODAS as rotas 🔑:
headers['Authorization'] = 'Bearer $idToken';
```
> O token expira em ~1h. Use `getIdToken()` antes de cada chamada (o SDK renova sozinho) ou trate `401` renovando e repetindo.

**4. Resolver o ID_CLIENTE após o login**
```text
GET /me
  └─ bemol_client_id == ""  → mostrar tela "informe seu ID_CLIENTE"
                               → POST /me/client-id { "bemol_client_id": "131" }
  └─ bemol_client_id != ""  → seguir direto para o feed
```
O usuário pode **editar** depois: basta chamar `POST /me/client-id` de novo.

**5. Carregar o feed**
```text
GET /recommend?k=20   (com Bearer)
```

**6. Registrar interações**
```text
POST /events    (com Bearer)  → batch de eventos (view, like, skip, ...)
POST /clickout  (com Bearer)  → quando o usuário toca pra comprar/abrir o produto
```
Guarde `rank` e `propensity` que vieram no `/recommend` e devolva-os nos eventos
(necessário para avaliação off-policy do modelo). Use um `event_id` (UUID) por
evento para reenvio seguro (idempotência).

**Erros a tratar:** `401` (token ausente/expirado), `403` (email fora de `@bemol.com.br`),
`409` (sem `ID_CLIENTE` vinculado → pedir), `422` (`ID_CLIENTE` inexistente no índice).

---

## Artefatos esperados em `artifacts/`

| Arquivo | Conteúdo | Origem |
|---|---|---|
| `meta.json` | dims, n_clients, n_products, hybrid_alpha | `datascience/bmltok_model/export.py` |
| `client_ids.json` | array de IDs de cliente (ordem do `.bin`) | idem |
| `product_ids.json` | array de IDs de produto (ordem do `.bin`) | idem |
| `client_embeddings.bin` | float32 raw, shape (n_clients, dims) | idem |
| `product_embeddings.bin` | float32 raw, shape (n_products, dims) | idem |
| `product_meta.json` | mapping `product_id → {name, category, price}` | idem |
| `product_popularity.bin` | float32 normalizado, shape (n_products,) | idem |
| `email_cli.hashed.csv` *(opcional)* | `HMAC-SHA256(email);ID_CLIENTE` p/ auto-link | `api/tools/hashdir/` |

No Cloud Run os artefatos são montados via **volume do GCS** (não vão na imagem).
Para gerar/atualizar localmente:
```bash
cd /home/david.camurca@bemol.local/Developer/bmltok/datascience
.venv/bin/python bmltok_model/export.py
```

---

## Rodar local

### Via Go (dev)
```bash
cd server
ARTIFACTS_DIR=../artifacts go run .
# http://localhost:8080/health
```
Sem Firebase configurado (ADC), sobe em **modo degradado**: serve `/recommend`
e `/similar`, mas `/me`, `/events`, `/clickout`, `/trends` e auth por token ficam off.

### Build/validação via Docker (sem Go instalado)
```bash
cd server
docker run --rm -v "$PWD":/app -w /app \
  --user "$(id -u):$(id -g)" \
  -e HOME=/tmp -e GOCACHE=/tmp/.cache -e GOPATH=/tmp/go \
  golang:1.23 sh -c "go mod tidy && go build ./..."
```

---

## Deploy (Cloud Run)

```bash
cd api
gcloud run deploy bemoltok-bff \
  --source . \
  --region southamerica-east1 \
  --project bemoltok-dev \
  --update-env-vars AUTH_REQUIRED=true,ALLOWED_EMAIL_DOMAIN=bemol.com.br,GOOGLE_CLOUD_PROJECT=bemoltok-dev \
  --update-secrets=VTEX_APP_KEY=vtex-app-key:latest,VTEX_APP_TOKEN=vtex-app-token:latest
```
O build é remoto (Cloud Build), então compila ARM→amd64 sem dor. Memória/CPU e o
volume do GCS são preservados entre deploys.

> **Rollback rápido do legado** (sem rebuild), só se o app real começar a tomar `401`:
> `gcloud run services update bemoltok-bff --region southamerica-east1 --update-env-vars AUTH_REQUIRED=false`

**IAM da service account do Cloud Run** (one-time):
- `roles/datastore.user` — acesso ao Firestore.
- `roles/iam.serviceAccountTokenCreator` (sobre si mesma) — assinar o **custom token** do OTP (`/auth/verify-code`). Sem isso, `verify-code` falha ao mintar o token.

---

## Configuração (variáveis de ambiente)

| Variável | Default | Descrição |
|---|---|---|
| `PORT` | `8080` | porta HTTP |
| `ARTIFACTS_DIR` | `/data/artifacts` | onde estão os `.bin`/`.json` (volume GCS no Cloud Run) |
| `GOOGLE_CLOUD_PROJECT` | — | projeto p/ Firebase Auth + Firestore (auto no Cloud Run) |
| `AUTH_REQUIRED` | `false` *(prod: `true`)* | `true` exige token em todas as rotas protegidas (desliga o legado). **Hoje roda `true`.** |
| `ALLOWED_EMAIL_DOMAIN` | — | restringe login a um domínio (ex.: `bemol.com.br`) |
| `ADMIN_TOKEN` | — | segredo do `POST /admin/reload` (via header `X-Admin-Token`). Vazio → endpoint **desabilitado** (503) |
| **Auto-link (diretório email → ID_CLIENTE)** | | |
| `DIRECTORY_FILE` | `<artifacts>/email_cli.hashed.csv` | caminho do diretório hasheado. Ausente → auto-link off |
| `DIRECTORY_HMAC_SECRET` | — | segredo HMAC usado p/ hashear o email no lookup (igual ao usado p/ gerar o arquivo) |
| **Magic link / SendGrid** | | |
| `SENDGRID_API_KEY` | — | chave do SendGrid. Vazio → `POST /auth/send-link` **desabilitado** (503) |
| `SENDGRID_FROM` | `inteligenciadenegocios@bemol.com.br` | remetente (domínio autenticado no SendGrid) |
| `SENDGRID_FROM_NAME` | `BemolTok` | nome de exibição do remetente |
| `SENDGRID_SUBJECT` | `Seu acesso ao BemolTok` | assunto do email |
| `SEND_LINK_APP_KEY` | — | quando setada, `POST /auth/send-link` exige o header `X-App-Key`. Vazio → endpoint aberto |
| `AUTH_CONTINUE_URL` | `https://bemoltok-dev.firebaseapp.com/finishSignIn` | `ActionCodeSettings.URL` (deve casar com o app) |
| `AUTH_IOS_BUNDLE` | `com.davidcamurca.bemoltok` | bundle iOS do link |
| `AUTH_ANDROID_PACKAGE` | `com.example.bemoltok` | package Android do link |
| **UGC (posts foto/vídeo)** | | |
| `UGC_BUCKET` | `bemoltok-dev-ugc` | bucket GCS privado da mídia |
| `UGC_SIGNER_SA` | — | email da SA de runtime, usada como `GoogleAccessID` p/ assinar V4 (IAM `signBlob`). Vazio → `/posts` **desabilitado** |
| **VTEX catalog proxy** | | |
| `VTEX_APP_KEY` | Secret Manager (`vtex-app-key`) | header `X-VTEX-API-AppKey`. Ausente → `/catalog/*` **503** |
| `VTEX_APP_TOKEN` | Secret Manager (`vtex-app-token`) | header `X-VTEX-API-AppToken`. Ausente → `/catalog/*` **503** |
| `VTEX_HOST` | `bemol.vtexcommercestable.com.br` | host VTEX upstream (opcional) |

> Eventos, contadores (UCB1), trends, perfis e experiments vivem todos no **Firestore**.
> Não há mais Postgres.
>
> **Segredos** (`ADMIN_TOKEN`, `DIRECTORY_HMAC_SECRET`, `SENDGRID_API_KEY`,
> `SEND_LINK_APP_KEY`, `VTEX_APP_KEY`, `VTEX_APP_TOKEN`) ficam no **Google Secret Manager**
> e são montados como env no Cloud Run via `--update-secrets` (os arquivos `.secret` locais
> servem de backup/rotação,
> fora do repo). A SA de runtime tem `roles/iam.serviceAccountTokenCreator` (para
> `signBlob` das signed URLs e dos custom tokens) e `roles/storage.objectAdmin` no bucket UGC.

---

## Performance

- Startup: carrega ~2 GB de embeddings (volume GCS) → Cloud Run com 8 GiB / 4 vCPU, `min-instances=1`.
- Latência `/recommend`: ~10 ms (FUSE/Cloud Run) · 3-8 ms local.
- Concorrência: `runtime.NumCPU()` workers por request, top-K via min-heap.

---

## Roadmap

- [x] Hot-reload de artefatos sem restart (`POST /admin/reload`, protegido por `X-Admin-Token`)
- [x] **Fase 1** — motor de recomendação no Cloud Run (volume GCS)
- [x] **Fase 2** — Firebase Auth (email-link) + perfil em Firestore (`users/{uid}`)
- [x] **Fase 3** — eventos + `/clickout` + `/trends` em Firestore; UCB1 alimentado por contadores
- [x] **Magic link branded** via SendGrid (`POST /auth/send-link`) + chave de app (`X-App-Key`)
- [x] **Login por código (OTP)** — `POST /auth/request-code` + `/auth/verify-code` (custom token; remove deep links do app)
- [x] **Auto-link** `email → ID_CLIENTE` via diretório hasheado (HMAC)
- [x] **Hardening** — regras Firestore em lockdown + segredos via env (`.secret` fora do repo)
- [x] `AUTH_REQUIRED=true` — legado desligado (app migrado p/ Bearer nas 4 rotas)
- [x] `/health` enxuto (sem `clients`/`products`/`directory_size`) + corpo de POST limitado a 1 MiB + batch de `/events` capado em 500
- [x] **Segredos no Secret Manager** (`--set-secrets`), fora de env em texto puro
- [x] **UGC (posts)** — fotos/vídeo por usuário via signed URLs (`POST /posts` + `/complete` + `GET /posts`)
- [ ] **App Check** (atestar app legítimo, acima da chave compartilhada)
- [ ] **Ownership do `ID_CLIENTE`** — exigir vínculo verificado via diretório (fecha o IDOR)
- [ ] **Moderação de UGC** — `status=pending_review` antes de entrar em feed
- [ ] **Distribuição do UGC por similaridade** — posts no feed de outros usuários (reusa embeddings de produto)
- [ ] Rate-limit distribuído (hoje in-memory por instância)
- [ ] Trends com janela temporal/decay em Memorystore Redis (quando o volume justificar)
- [ ] **Fase 4/5** — feed agregado e UGC (posts/comentários)
- [ ] Otimizar loader de embeddings (evitar alocação dupla → cair de 8 GiB p/ 4 GiB)

---

## Projeto irmão

O modelo (treino + exportação de embeddings) vive em:
```
/home/david.camurca@bemol.local/Developer/bmltok/datascience/
```
