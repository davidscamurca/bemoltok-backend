# Mapeamento Completo de Rotas da API (BFF BemolTok)

Este documento descreve detalhadamente todas as rotas HTTP expostas pelo **BFF (Backend for Frontend)** do BemolTok, desenvolvido em Go e executado no Google Cloud Run (`bemoltok-bff`).

---

## 1. Visão Geral da API

- **Serviço**: `bemoltok-bff` (Google Cloud Run)
- **Região**: `southamerica-east1` (São Paulo)
- **URL Padrão (Cloud Run)**: `https://bemoltok-bff-bium6anima-rj.a.run.app`
- **Tamanho Máximo de Requisição**: `1 MiB` (`maxRequestBody = 1 << 20`)
- **Padrão de Respostas de Erro**: Formato JSON padrão `{"error": "descrição do erro"}`

### Camadas de Segurança e Autenticação
1. **Públicas**: Endpoints de status operacional ou visualização anônima (`/health`, `/similar/`, `/trends`).
2. **Protegidas por Chave de App (`X-App-Key`)**: Endpoints de pré-login (`/auth/request-code`, `/auth/verify-code`, `/auth/send-link`) que exigem a chave de aplicação para prevenir abuso de envio de emails.
3. **Autenticadas por Bearer Token (`Authorization: Bearer <ID_TOKEN>`)**: Validam o Firebase ID Token através do Firebase Admin SDK (`srv.fb.Auth.VerifyIDToken(ctx, token)`). O domínio do e-mail é validado contra `ALLOWED_EMAIL_DOMAIN` (`@bemol.com.br`).
4. **Protegidas por Chave Administrativa (`X-Admin-Token`)**: Endpoints de operação interna e recarga de modelos ML (`/admin/reload`).

---

## 2. Autenticação e Sessão

### 2.1. Solicitar Código OTP
- **Método**: `POST`
- **Rota**: `/auth/request-code`
- **Autenticação**: Header obrigatório `X-App-Key: <SEND_LINK_APP_KEY>`
- **Descrição**: Gera um código numérico de 6 dígitos aleatório, calcula o HMAC do código associado ao e-mail, armazena no Firestore com validade de 10 minutos e dispara o e-mail estilizado através da API do SendGrid. Aplica rate limiting por e-mail e IP.
- **Request Headers**:
  - `Content-Type: application/json`
  - `X-App-Key: <chave-da-app>`
- **Request Body**:
  ```json
  {
    "email": "colaborador@bemol.com.br"
  }
  ```
- **Respostas**:
  - `204 No Content`: Código gerado e enviado com sucesso (ou e-mail mascarado por segurança).
  - `400 Bad Request`: `{"error": "invalid email"}`
  - `401 Unauthorized`: `{"error": "unauthorized"}` (falha de validação do `X-App-Key`).
  - `403 Forbidden`: `{"error": "email domain not allowed"}` (e-mail fora de `@bemol.com.br`).
  - `429 Too Many Requests`: `{"error": "too many requests; try again shortly"}`
  - `502 Bad Gateway`: `{"error": "could not send email"}` (falha no SendGrid).
  - `503 Service Unavailable`: `{"error": "otp not configured"}`

---

### 2.2. Validar Código OTP e Obter Sessão
- **Método**: `POST`
- **Rota**: `/auth/verify-code`
- **Autenticação**: Header obrigatório `X-App-Key: <SEND_LINK_APP_KEY>`
- **Descrição**: Verifica a validade do código informado contra o hash HMAC armazenado em `auth_codes/{emailHMAC}`. Se correto, consome o código (impede reutilização), busca ou cria o usuário no Firebase Auth (`EmailVerified: true`) e emite um **Firebase Custom Token**. O aplicativo Flutter troca este token pelo ID Token definitivo via `signInWithCustomToken`.
- **Request Headers**:
  - `Content-Type: application/json`
  - `X-App-Key: <chave-da-app>`
- **Request Body**:
  ```json
  {
    "email": "colaborador@bemol.com.br",
    "code": "849201"
  }
  ```
- **Respostas**:
  - `200 OK`:
    ```json
    {
      "custom_token": "eyJhbGciOiJSUzI1NiIsImtpZCI6Ij..."
    }
    ```
  - `400 Bad Request`: `{"error": "invalid email or code"}`
  - `401 Unauthorized`: `{"error": "invalid or expired code"}` ou `{"error": "too many attempts; request a new code"}`
  - `403 Forbidden`: `{"error": "email domain not allowed"}`
  - `500 Internal Server Error`: `{"error": "could not mint token"}`

---

### 2.3. Envio de Magic Link (Legado)
- **Método**: `POST`
- **Rota**: `/auth/send-link`
- **Autenticação**: Header `X-App-Key`
- **Descrição**: Gera um link de autenticação via Firebase Hosting (`finishSignIn.html`) e dispara por e-mail. Mantido para compatibilidade retroativa.

---

## 3. Perfil de Usuário e Identidade

### 3.1. Obter Perfil Atual do Usuário
- **Método**: `GET`
- **Rota**: `/me`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Retorna o documento do usuário logado (`users/{uid}`). Na primeira requisição do usuário, cria o registro automaticamente. Se o usuário ainda não tiver um `bemol_client_id` vinculado, tenta realizar a vinculação automática (`auto-link`) a partir do diretório HMAC pré-computado de e-mails corporativos.
- **Response `200 OK`**:
  ```json
  {
    "uid": "ABcd1234EFgh5678IJkl",
    "email": "nelson.pinto@bemol.com.br",
    "bemol_client_id": "902184",
    "client_id_verified": true,
    "created_at": "2026-09-10T14:22:00Z",
    "last_seen_at": "2026-09-28T05:12:00Z"
  }
  ```
- **Respostas de Erro**:
  - `401 Unauthorized`: `{"error": "authentication required"}`
  - `503 Service Unavailable`: `{"error": "auth not configured"}`

---

### 3.2. Vincular Manualmente `ID_CLIENTE` Bemol
- **Método**: `POST`
- **Rota**: `/me/client-id`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Vincula manualmente um `ID_CLIENTE` da Bemol ao perfil autenticado. O backend valida obrigatoriamente se esse cliente existe no índice vetorial de machine learning em memória. Caso não exista no modelo, a vinculação é recusada para evitar erros nas recomendações.
- **Request Body**:
  ```json
  {
    "bemol_client_id": "902184"
  }
  ```
- **Respostas**:
  - `200 OK`: Retorna o `UserProfile` atualizado.
  - `400 Bad Request`: `{"error": "missing bemol_client_id"}`
  - `422 Unprocessable Entity`: `{"error": "ID_CLIENTE not found in recommendation index"}`

---

### 3.3. Perfil Público de Outro Usuário
- **Método**: `GET`
- **Rota**: `/users/{uid}`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Retorna a projeção pública de um criador de conteúdo no app. Por privacidade, omite o e-mail real e o `bemol_client_id`, retornando o rótulo de autor (`display_label`), contagem de publicações e total de curtidas recebidas.
- **Response `200 OK`**:
  ```json
  {
    "uid": "criador_xyz_123",
    "display_label": "joao.silva",
    "avatar_url": "",
    "posts_count": 8,
    "likes_received_count": 142
  }
  ```

---

### 3.4. Listar Publicações de um Usuário Específico
- **Método**: `GET`
- **Rota**: `/users/{uid}/posts`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Retorna todas as postagens publicadas pelo criador indicado por `{uid}`, contendo URLs assinadas de visualização do Google Cloud Storage com validade de 60 minutos.
- **Response `200 OK`**:
  ```json
  {
    "posts": [
      {
        "id": "post_789",
        "product_id": "224958",
        "type": "video",
        "caption": "Testando a Airfryer no almoço!",
        "author_label": "joao.silva",
        "like_count": 15,
        "created_at": "2026-09-25T18:00:00Z",
        "published_at": "2026-09-25T18:02:15Z",
        "media_urls": [
          "https://storage.googleapis.com/bemoltok-dev-ugc/posts/criador_xyz_123/post_789/0.mp4?X-Goog-Algorithm=..."
        ]
      }
    ]
  }
  ```

---

## 4. Feed Misto e Motor de Recomendação

### 4.1. Feed Principal Misto (Produtos + UGC Reviews)
- **Método**: `GET`
- **Rota**: `/feed`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Query Parameters**:
  - `cursor` (int, default `0`): Índice de paginação.
  - `limit` (int, default `40`, máx `200`): Quantidade de itens por página.
- **Descrição**:
  1. Resolve o `bemol_client_id` do usuário logado.
  2. Executa a inferência vetorial para os top produtos ranqueados por ALS + Popularidade + UCB1.
  3. Varre os posts UGC publicados (`status=published`) relacionados ao conjunto de produtos recomendados ou seus similares mais próximos.
  4. Valida se o produto do post possui snapshot válido (nome e preço maiores que zero).
  5. Intercala posts UGC com produtos de vitrine na proporção de **1 post a cada 4 produtos** (`feedDefaultPostGap = 4`), desduplicando autor e produto consecutivos.
- **Response `200 OK`**:
  ```json
  {
    "client_id": "902184",
    "cursor": 0,
    "next_cursor": 40,
    "has_more": true,
    "items": [
      {
        "type": "product",
        "product_id": "224958",
        "rank": 1,
        "score": 0.8942,
        "name": "Fritadeira Sem Óleo Air Fryer Mondial 4L",
        "price": 389.90,
        "category": "Eletroportáteis",
        "is_new": false,
        "propensity": 0.124,
        "product_snapshot": {
          "name": "Fritadeira Sem Óleo Air Fryer Mondial 4L",
          "image_url": "https://bemol.vteximg.com.br/arquivos/ids/...",
          "price": 389.90,
          "detail_url": "https://www.bemol.com.br/224958/p"
        }
      },
      {
        "type": "post",
        "post_id": "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d",
        "author_uid": "user_abc",
        "author_label": "maria.souza",
        "product_id": "224958",
        "caption": "Melhor compra que fiz esse mês! Super silenciosa.",
        "post_type": "video",
        "media_urls": [
          "https://storage.googleapis.com/bemoltok-dev-ugc/posts/user_abc/9b1deb4d/0.mp4?..."
        ],
        "published_at": "2026-09-27T12:00:00Z",
        "eligibility": "pool",
        "product_snapshot": {
          "name": "Fritadeira Sem Óleo Air Fryer Mondial 4L",
          "image_url": "https://bemol.vteximg.com.br/arquivos/ids/...",
          "price": 389.90,
          "detail_url": "https://www.bemol.com.br/224958/p"
        }
      }
    ]
  }
  ```
- **Respostas de Erro**:
  - `401 Unauthorized`: Sem token de autenticação.
  - `409 Conflict`: `{"error": "no ID_CLIENTE linked; call POST /me/client-id first"}`

---

### 4.2. Recomendações Algorítmicas Puras
- **Métodos**: `GET /recommend` (usuário logado via token) e `GET /recommend/{clientId}` (legado com ID na URL)
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Query Parameters**:
  - `k` (int, default `10`, máx `200`): Quantidade de itens a ranquear.
- **Descrição**: Calcula o produto escalar vetorial (dot product desenrolado de 8 em 8 dimensões via SIMD/CPU) entre o vetor do cliente e os embeddings de catálogo, somado à popularidade histórica e ao bônus de exploração do algoritmo UCB1.
- **Response `200 OK`**:
  ```json
  {
    "client_id": "902184",
    "recommendations": [
      {
        "rank": 1,
        "product_id": "224958",
        "name": "Fritadeira Sem Óleo Air Fryer Mondial",
        "score": 0.8942,
        "price": 389.90,
        "category": "Eletroportáteis",
        "is_new": false,
        "propensity": 0.124
      }
    ],
    "latency_ms": 3.42
  }
  ```

---

### 4.3. Produtos Similares (Item-to-Item)
- **Método**: `GET`
- **Rota**: `/similar/{productId}`
- **Autenticação**: Pública
- **Query Parameters**: `k` (default `10`, máx `200`)
- **Descrição**: Calcula o cosseno/dot product entre o embedding do item indicado e todo o catálogo em memória. Retorna os vizinhos mais próximos excluindo o próprio produto de origem.
- **Response `200 OK`**:
  ```json
  {
    "product_id": "224958",
    "similar": [
      {
        "rank": 1,
        "product_id": "218940",
        "name": "Fritadeira Elétrica Philco Air Fry",
        "score": 0.9412,
        "price": 359.00,
        "category": "Eletroportáteis",
        "is_new": false
      }
    ],
    "latency_ms": 2.85
  }
  ```

---

### 4.4. Produtos em Tendência (Trends)
- **Método**: `GET`
- **Rota**: `/trends`
- **Autenticação**: Pública
- **Query Parameters**: `k` (default `10`, máx `200`)
- **Descrição**: Lê a coleção `product_stats` do Firestore ordenada decrescentemente pelo campo `positive_signals` (soma acumulada ponderada de curtidas, favoritos, comentários e compartilhamentos).
- **Response `200 OK`**:
  ```json
  {
    "trends": [
      {
        "rank": 1,
        "product_id": "224958",
        "name": "Fritadeira Sem Óleo Air Fryer Mondial",
        "positive_signals": 128.5
      }
    ]
  }
  ```

---

## 5. Criação e Gestão de Publicações (UGC - User Generated Content)

### 5.1. Inicializar Criação de Post (Obter Signed URLs)
- **Método**: `POST`
- **Rota**: `/posts`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Valida os metadados da postagem, cria um registro provisório no Firestore com status `awaiting_upload` e gera URLs V4 assinadas de `PUT` diretamente para o Google Cloud Storage (`bemoltok-dev-ugc`). A mídia nunca transita pela CPU do Cloud Run.
- **Regras de Negócio**:
  - `product_id`: Obrigatório.
  - `caption`: Máximo de 500 caracteres (runas UTF-8).
  - Tipo `photos`: Exige entre 3 e 5 itens (`image/jpeg`, `image/png`), máx 10 MiB por foto.
  - Tipo `video`: Exige exatamente 1 item (`video/mp4`, `video/quicktime`), máx 100 MiB e duração até 120s.
- **Request Body**:
  ```json
  {
    "product_id": "224958",
    "type": "video",
    "caption": "Review completo desse produto incrível!",
    "items": [
      {
        "content_type": "video/mp4"
      }
    ]
  }
  ```
- **Response `201 Created`**:
  ```json
  {
    "post_id": "b3f2e1a0-9c8d-4e5f-b1a2-3c4d5e6f7a8b",
    "status": "awaiting_upload",
    "upload_expires_at": "2026-09-28T05:30:00Z",
    "max_video_seconds": 120,
    "uploads": [
      {
        "object": "posts/user_uid/b3f2e1a0-9c8d-4e5f-b1a2-3c4d5e6f7a8b/0.mp4",
        "content_type": "video/mp4",
        "method": "PUT",
        "upload_url": "https://storage.googleapis.com/bemoltok-dev-ugc/posts/user_uid/b3f2e1a0/0.mp4?X-Goog-Algorithm=...",
        "headers": {
          "Content-Type": "video/mp4"
        },
        "max_bytes": 104857600
      }
    ]
  }
  ```

---

### 5.2. Confirmar Upload e Publicar Post
- **Método**: `POST`
- **Rota**: `/posts/{id}/complete`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: O aplicativo chama esta rota após concluir com sucesso os uploads `PUT` no Cloud Storage. O backend inspeciona os metadados dos objetos no GCS, valida se existem e respeitam os limites de tamanho e tipo, define o status para `published`, registra `published_at` e atualiza a postagem. É idempotente.
- **Response `200 OK`**:
  ```json
  {
    "id": "b3f2e1a0-9c8d-4e5f-b1a2-3c4d5e6f7a8b",
    "product_id": "224958",
    "type": "video",
    "caption": "Review completo desse produto incrível!",
    "status": "published",
    "created_at": "2026-09-28T05:15:00Z",
    "published_at": "2026-09-28T05:16:30Z",
    "author_label": "nelson.pinto",
    "like_count": 0,
    "media": [
      {
        "object": "posts/user_uid/b3f2e1a0/0.mp4",
        "content_type": "video/mp4",
        "size": 45210982
      }
    ]
  }
  ```
- **Respostas de Erro**:
  - `400 Bad Request`: `{"error": "upload incomplete: missing posts/..."}`
  - `413 Request Entity Too Large`: `{"error": "... exceeds the byte limit"}` (objeto excluído preventivamente do bucket).

---

### 5.3. Listar Minhas Publicações
- **Método**: `GET`
- **Rota**: `/posts`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Retorna todas as publicações já concluídas pertencentes ao próprio usuário logado, com URLs assinadas de visualização (`viewURLTTL = 60m`).
- **Response `200 OK`**:
  ```json
  {
    "posts": [
      {
        "id": "b3f2e1a0-...",
        "product_id": "224958",
        "type": "video",
        "caption": "Review...",
        "media_urls": ["https://storage.googleapis.com/..."]
      }
    ]
  }
  ```

---

### 5.4. Obter Detalhes de um Post Específico
- **Método**: `GET`
- **Rota**: `/posts/{id}`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Retorna os detalhes de uma publicação específica incluindo se o usuário logado a curtiu (`liked_by_me`).
- **Response `200 OK`**:
  ```json
  {
    "post": {
      "id": "b3f2e1a0-...",
      "product_id": "224958",
      "type": "video",
      "caption": "Review...",
      "like_count": 12,
      "media_urls": ["https://..."]
    },
    "liked_by_me": true
  }
  ```

---

## 6. Recursos Sociais: Curtidas, Favoritos e Comentários

### 6.1. Curtidas e Favoritos em Produtos

#### Listar Produtos Curtidos pelo Usuário
- **Método**: `GET`
- **Rota**: `/me/likes`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Response `200 OK`**: `{"product_ids": ["224958", "190283"]}`

#### Curtir um Produto
- **Método**: `PUT`
- **Rota**: `/me/likes/{productId}`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Response `200 OK`**: `{"status": "ok"}`

#### Descurtir um Produto
- **Método**: `DELETE`
- **Rota**: `/me/likes/{productId}`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Response `200 OK`**: `{"status": "ok"}`

#### Listar Favoritos (Bookmarks) do Usuário
- **Método**: `GET`
- **Rota**: `/me/bookmarks`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Response `200 OK`**: `{"product_ids": ["224958"]}`

#### Adicionar aos Favoritos
- **Método**: `PUT`
- **Rota**: `/me/bookmarks/{productId}`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Response `200 OK`**: `{"status": "ok"}`

#### Remover dos Favoritos
- **Método**: `DELETE`
- **Rota**: `/me/bookmarks/{productId}`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Response `200 OK`**: `{"status": "ok"}`

---

### 6.2. Curtidas em Postagens UGC

#### Curtir Post
- **Método**: `PUT`
- **Rota**: `/posts/{id}/like`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Executa transação atômica no Firestore: adiciona o documento em `posts/{id}/likes/{uid}` e incrementa `like_count` no post.
- **Response `200 OK`**: `{"status": "ok"}`

#### Remover Curtida de Post
- **Método**: `DELETE`
- **Rota**: `/posts/{id}/like`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Remove `posts/{id}/likes/{uid}` e decrementa `like_count` atomicamente (sem baixar de zero).
- **Response `200 OK`**: `{"status": "ok"}`

---

### 6.3. Comentários em Postagens UGC (`post_comments`)

#### Listar Comentários de um Post
- **Método**: `GET`
- **Rota**: `/posts/{id}/comments`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Response `200 OK`**:
  ```json
  {
    "comments": [
      {
        "id": "c1a2b3c4-...",
        "author_label": "carlos.m",
        "post_id": "post_789",
        "text": "Ficou muito bom o vídeo!",
        "created_at": "2026-09-28T03:00:00Z",
        "like_count": 2,
        "liked_by_me": false,
        "status": "published"
      }
    ]
  }
  ```

#### Publicar Comentário em Post
- **Método**: `POST`
- **Rota**: `/posts/{id}/comments`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Request Body**:
  ```json
  {
    "text": "Muito boa essa dica!",
    "parent_id": ""
  }
  ```
- **Response `201 Created`**: Retorna o objeto do comentário criado.

#### Curtir / Descurtir Comentário de Post
- **Curtir**: `PUT /posts/{id}/comments/{commentId}/like`
- **Descurtir**: `DELETE /posts/{id}/comments/{commentId}/like`
- **Response `200 OK`**: `{"status": "ok"}`

---

### 6.4. Comentários em Produtos do Catálogo (`product_comments`)

#### Listar Comentários de um Produto
- **Método**: `GET`
- **Rota**: `/products/{productId}/comments`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Response `200 OK`**:
  ```json
  {
    "comments": [
      {
        "id": "comm_123",
        "author_label": "ana.lima",
        "product_id": "224958",
        "parent_id": "",
        "text": "A voltagem é 110V ou 220V?",
        "created_at": "2026-09-27T10:00:00Z",
        "like_count": 3,
        "liked_by_me": true,
        "status": "published"
      },
      {
        "id": "comm_124",
        "author_label": "vendedor.bemol",
        "product_id": "224958",
        "parent_id": "comm_123",
        "text": "Olá! Temos disponível em 110V e 220V no site.",
        "created_at": "2026-09-27T10:15:00Z",
        "like_count": 1,
        "liked_by_me": false,
        "status": "published"
      }
    ]
  }
  ```

#### Publicar Comentário ou Resposta em Produto
- **Método**: `POST`
- **Rota**: `/products/{productId}/comments`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Segurança**: Rejeita qualquer requisição em que o cliente tente injetar campos de moderação ou identidade (`uid`, `email`, `status`, `like_count`, `reported_count`, etc.). O texto é limitado a 2000 caracteres.
- **Request Body**:
  ```json
  {
    "text": "A entrega para Manaus demorou quanto tempo?",
    "parent_id": ""
  }
  ```
- **Response `201 Created`**: Objeto do comentário criado.

#### Curtir / Descurtir Comentário em Produto
- **Curtir**: `PUT /products/{productId}/comments/{commentId}/like`
- **Descurtir**: `DELETE /products/{productId}/comments/{commentId}/like`
- **Response `200 OK`**: `{"status": "ok"}`

---

## 7. Catálogo e Integração VTEX

O BFF atua como proxy seguro para a VTEX. As chaves `X-VTEX-API-AppKey` e `X-VTEX-API-AppToken` residem exclusivamente nas variáveis de ambiente do Cloud Run, nunca trafegando no aplicativo móvel.

### 7.1. Consulta Detalhada de SKU por AlternateId / RefId
- **Método**: `GET`
- **Rota**: `/catalog/sku/{refId}`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**:
  1. Consulta a API privada de SKU da VTEX (`/api/catalog_system/pvt/sku/stockkeepingunitbyalternateId/{refId}`).
  2. Caso o payload original não traga preço (`CommertialOffer`), fotos ou links da loja, faz uma busca enriquecida na API de busca pública (`/api/catalog_system/pub/products/search/`) por `alternateIds_RefId:{refId}` ou `productId:{refId}`.
  3. Realiza o merge das informações visuais e comerciais (`mergeSKUVisuals`), garantindo que o card na interface exiba imagem, preço de venda e link de redirecionamento.
- **Response `200 OK`**:
  ```json
  {
    "ProductId": 10582,
    "NameComplete": "Smartphone Motorola Moto G35 5G 128GB",
    "ProductDescription": "O novo Moto G35 5G combina desempenho...",
    "ProductRefId": "224958",
    "BrandName": "Motorola",
    "ImageUrl": "https://bemol.vteximg.com.br/arquivos/ids/...",
    "Images": [
      {"ImageUrl": "https://bemol.vteximg.com.br/arquivos/ids/..."}
    ],
    "DetailUrl": "/smartphone-motorola-moto-g35-128gb/p",
    "Sellers": [
      {
        "SellerId": "1",
        "CommertialOffer": {
          "Price": 1299.00,
          "ListPrice": 1499.00,
          "AvailableQuantity": 42
        }
      }
    ]
  }
  ```

---

### 7.2. Busca e Filtro de Catálogo
- **Método**: `GET`
- **Rota**: `/catalog/search`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Query Parameters**:
  - `_from` (int): Início da paginação (e.g. `0`).
  - `_to` (int): Fim da paginação (e.g. `19`).
  - `fq` (string): Filtros de busca (ex: `fq=C:1000001` ou `fq=alternateIds_RefId:224958`).
  - `O` (string): Ordenação (ex: `OrderByTopSaleDESC`, `OrderByPriceASC`).
- **Descrição**: Encaminha os parâmetros de busca para o catálogo da VTEX aplicando as credenciais no servidor e normalizando parâmetros legados.
- **Response `200 OK` ou `206 Partial Content`**: Retorna a lista de produtos da VTEX.

---

## 8. Telemetria e Exploração Online (UCB1)

### 8.1. Ingestão em Lote de Eventos de Interação
- **Método**: `POST`
- **Rota**: `/events`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Request Headers**:
  - `Content-Type: application/json`
  - `X-Client-Version: 1.0.0`
- **Descrição**: Recebe um lote de até 500 eventos coletados no aplicativo (visualizações, curtidas, cliques, compartilhamentos, tempo de tela). As gravações no Firestore (`events/{eventId}`) são **idempotentes**: eventos repetidos são ignorados e não causam duplicação de métricas. Cada evento válido incrementa atomicamente os contadores em `product_stats/{productId}` para alimentar o modelo de bandit UCB1.
- **Tipos de Eventos com Sinal Positivo (`positive_signals`)**: `like`, `bookmark`, `share`, `click_out`, `comment`.
- **Request Body**:
  ```json
  {
    "events": [
      {
        "event_id": "7f8b9a1c-...",
        "product_id": "224958",
        "category": "Eletroportáteis",
        "kind": "like",
        "value": 1.0,
        "at": "2026-09-28T05:20:00Z",
        "rank": 2,
        "propensity": 0.085,
        "session_id": "sess_123",
        "impression_id": "imp_456"
      },
      {
        "event_id": "8a9b0c1d-...",
        "product_id": "224958",
        "category": "Eletroportáteis",
        "kind": "view",
        "value": 1.0,
        "at": "2026-09-28T05:20:01Z"
      }
    ]
  }
  ```
- **Response `200 OK`**:
  ```json
  {
    "inserted": 2
  }
  ```

---

### 8.2. Registro Direto de Conversão (Clickout)
- **Método**: `POST`
- **Rota**: `/clickout`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Atalho para registrar o clique de abertura do produto na loja Bemol (sinal de maior valor no funil de conversão).
- **Request Body**:
  ```json
  {
    "product_id": "224958",
    "value": 1.0
  }
  ```
- **Response `200 OK`**: `{"status": "ok", "recorded": true}`

---

## 9. Testes A/B e Experimentos

### 9.1. Obter Variantes Ativas de Experimentos
- **Método**: `GET`
- **Rota**: `/experiments`
- **Autenticação**: `Authorization: Bearer <ID_TOKEN>`
- **Descrição**: Lê os testes ativos no Firestore (`experiments/{expId}`). Calcula um bucket determinístico no intervalo `[0, 1)` através de:
  $$\text{bucket} = \frac{\text{uint32}(\text{SHA-256}(\text{userID} + \text{":"} + \text{experimentID})[:4])}{2^{32} - 1}$$
  Retorna os parâmetros da variante correspondente ao bucket do usuário.
- **Response `200 OK`**:
  ```json
  {
    "experiments": [
      {
        "experiment_id": "exp_feed_gap_v1",
        "variant": "dense",
        "params": {
          "gap": 3
        }
      }
    ]
  }
  ```

---

## 10. Operações e Monitoramento

### 10.1. Health Check
- **Método**: `GET`
- **Rota**: `/health`
- **Autenticação**: Pública
- **Descrição**: Verifica a integridade da instância do Cloud Run, tempo decorrido desde a última atualização das estatísticas UCB1 e status da conexão com o Firestore.
- **Response `200 OK`**:
  ```json
  {
    "status": "ok",
    "stats_age": "2m30s",
    "firestore_connected": true
  }
  ```

---

### 10.2. Recarga Dinâmica de Artefatos ML (Hot-Swap)
- **Método**: `POST`
- **Rota**: `/admin/reload`
- **Autenticação**: Header obrigatório `X-Admin-Token: <ADMIN_TOKEN>` (comparado em tempo constante contra `ADMIN_TOKEN`)
- **Descrição**: Recarrega os arquivos de embeddings (`client_embeddings.bin`, `item_embeddings.bin`, `meta.json`, `product_meta.json`) da pasta montada de artefatos para a memória RAM sem necessidade de reiniciar a aplicação ou causar indisponibilidade para os usuários.
- **Response `200 OK`**:
  ```json
  {
    "status": "ok",
    "clients": 158200,
    "products": 4210,
    "dims": 64,
    "duration_ms": 128.4
  }
  ```
