# Fluxos de Negócio, Arquitetura e Funcionamento do Sistema

Este documento descreve detalhadamente o funcionamento interno, algoritmos matemáticos e fluxos operacionais de ponta a ponta do **BemolTok**.

---

## 1. Fluxo de Autenticação Passwordless via OTP

O BemolTok adota autenticação sem senha (passwordless) restrita ao domínio corporativo `@bemol.com.br`, garantindo conformidade com a política de acesso interno e sem exigir gerenciamento de senhas pelos colaboradores.

```mermaid
sequenceDiagram
    autonumber
    actor User as Colaborador
    participant App as App Flutter
    participant BFF as Cloud Run (BFF)
    participant SG as SendGrid API
    participant FS as Firestore (auth_codes)
    participant FA as Firebase Auth
    participant US as Firestore (users)

    User->>App: Digita e-mail corporativo
    App->>BFF: POST /auth/request-code (X-App-Key, email)
    Note over BFF: Valida domínio @bemol.com.br e rate limiting
    BFF->>BFF: Gera OTP de 6 dígitos aleatório
    BFF->>FS: Grava HMAC(secret, email+code) com TTL 10 min
    BFF->>SG: Envia e-mail estilizado com o código
    BFF-->>App: 204 No Content
    SG-->>User: Recebe código na caixa postal

    User->>App: Digita código de 6 dígitos
    App->>BFF: POST /auth/verify-code (X-App-Key, email, code)
    BFF->>FS: Consulta auth_codes/{emailHMAC}
    Note over BFF: Valida HMAC em tempo constante e limite de tentativas
    BFF->>FS: Marca used=true (queima o código)
    BFF->>FA: GetOrCreateUserByEmail(email)
    BFF->>FA: Emite Custom Token (Admin SDK)
    BFF-->>App: 200 OK (custom_token)
    
    App->>FA: signInWithCustomToken(custom_token)
    FA-->>App: Sessão iniciada (Firebase ID Token)
    
    App->>BFF: GET /me (Bearer ID_TOKEN)
    BFF->>FA: VerifyIDToken(token)
    BFF->>US: Carrega ou cria users/{uid}
    Note over BFF: Auto-link: resolve ID_CLIENTE via directory HMAC
    BFF-->>App: 200 OK (UserProfile com bemol_client_id)
```

### Auto-renovação de Token no Frontend (`sendAuthed`)
Todas as chamadas autenticadas do Flutter utilizam a função utilitária `sendAuthed` localizada em [auth_http.dart](file:///Users/nelsonpinto/dev/BemolTok/bemoltok-front/lib/core/auth_http.dart).
- Se uma requisição retornar código `401 Unauthorized` (por expiração do token de 1 hora do Firebase), o cliente invoca `currentUser.getIdToken(forceRefresh: true)` automaticamente e reexecuta a requisição sem interromper a navegação do usuário.

---

## 2. Geração e Composição do Feed Misto (`GET /feed`)

O feed do BemolTok não exibe apenas produtos estáticos de vitrine, mas cria uma experiência de mídia contínua intercalando produtos de alta relevância com vídeos e fotos de reviews gerados por colaboradores (UGC).

```mermaid
flowchart TD
    Start([Requisição GET /feed]) --> Auth[Valida Bearer Token & Resolve bemol_client_id]
    Auth --> RecEngine[Motor ML em Memória: Calcula Top-K Produtos Ranqueados]
    RecEngine --> PoolSet[Pool de Produtos Recomendados]
    PoolSet --> SimilarSet[Calcula Top-5 Similares para os Produtos do Pool]
    
    PoolSet & SimilarSet --> QueryUGC[Varre Firestore: posts onde status == published]
    QueryUGC --> FilterUGC{Post associado ao Pool ou Similares?}
    FilterUGC -- Não --> DiscardPost[Descarta Post]
    FilterUGC -- Sim --> SnapCheck{Produto possui Snapshot Válido?}
    SnapCheck -- Sem preço ou nome --> DiscardPost
    SnapCheck -- Válido --> SignMedia[Gera URLs Assinadas V4 no GCS para Fotos/Vídeos]
    SignMedia --> SortUGC[Ordena Posts por Data de Publicação e Prioridade de Pool]
    
    SortUGC & PoolSet --> Interleave[Interleaving: 1 Post a cada 4 Produtos]
    Interleave --> Dedup{Mesmo autor ou produto em sequência?}
    Dedup -- Sim --> Swap[Ajusta posição no carrossel]
    Dedup -- Não --> FinalList[Monta Lista Paginada do Feed]
    FinalList --> ReturnJSON([Retorna JSON ao App Flutter])
```

### Regras do Interleaving do Feed:
1. **Razão de Proporção**: 1 post UGC para cada 4 produtos recomendados (`feedDefaultPostGap = 4`).
2. **Desduplicação de Autor**: Evita que dois vídeos ou fotos do mesmo criador apareçam colados na sequência do feed.
3. **Desduplicação de Produto**: Evita que o mesmo SKU seja exibido repetidas vezes consecutivas.
4. **Resiliência Visual**: Se um produto ou post não tiver imagem válida ou preço no catálogo, ele é descartado pelo filtro `productSnapshotUsable` para que o usuário nunca veja cards quebrados.

---

## 3. Ciclo de Vida de Upload UGC (Two-Step Upload Seguro)

Para que vídeos pesados (até 100 MiB) e fotos de alta resolução não sobrecarreguem as instâncias do Cloud Run e consumam banda desnecessária no backend, o BemolTok adota a estratégia de **upload direto no storage via URLs assinadas**:

```mermaid
sequenceDiagram
    autonumber
    actor Creator as Criador (App)
    participant App as App Flutter
    participant BFF as Cloud Run (BFF)
    participant GCS as Bucket bemoltok-dev-ugc
    participant FS as Firestore (posts)

    Creator->>App: Seleciona vídeo ou 3-5 fotos na galeria
    Creator->>App: Digita legenda e vincula produto
    App->>BFF: POST /posts (Bearer, product_id, type, caption, content_types)
    Note over BFF: Valida limites: fotos (3 a 5, max 10MB), vídeo (1, max 100MB, 120s)
    BFF->>FS: Grava doc posts/{id} com status="awaiting_upload"
    BFF->>BFF: Gera Signed PUT URLs (V4) com TTL de 15 minutos
    BFF-->>App: 201 Created (post_id, lista de URLs assinadas)

    loop Para cada arquivo de mídia
        App->>GCS: HTTP PUT direto com Content-Type original
        GCS-->>App: 200 OK (gravado no bucket)
    end

    App->>BFF: POST /posts/{id}/complete (Bearer)
    Note over BFF: Inspeciona Attrs no GCS: verifica se todos os objetos existem e respeitam limites
    BFF->>FS: Atualiza status="published", registra published_at e contadores
    BFF-->>App: 200 OK (PostDoc completo publicado)
    App-->>Creator: Notifica sucesso e exibe publicação no perfil
```

---

## 4. Algoritmo de Recomendação e Exploração UCB1

O motor de recomendação combina aprendizado supervisionado de matrizes esparsas com aprendizado por reforço para equilibrar itens consolidados e novos produtos em catálogo (**Exploitation vs. Exploration**).

### 4.1. Fórmula do Score de Ranqueamento

Para um cliente $u$ e um produto candidato $i$, o score final é computado por:

$$\text{Score}(u, i) = \alpha \cdot \langle \mathbf{e}_u, \mathbf{e}_i \rangle + (1 - \alpha) \cdot \text{Pop}_i + \gamma \cdot \text{UCB1}_i$$

Onde:
- $\mathbf{e}_u, \mathbf{e}_i \in \mathbb{R}^{64}$: Vetores latentes de embedding do cliente e do item, pré-computados via decomposição matricial (ALS) e normalizados.
- $\langle \mathbf{e}_u, \mathbf{e}_i \rangle$: Produto escalar vetorial computado em memória com operações SIMD desenroladas de 8 em 8 dimensões.
- $\text{Pop}_i \in [0, 1]$: Popularidade histórica normalizada das vendas do produto.
- $\alpha = 0.7$: Parâmetro de peso híbrido configurado em `meta.json`.
- $\gamma = 0.2$: Peso da exploração ativa de novidades (`ucbGamma = float32(0.2)`).

### 4.2. Bônus de Exploração UCB1 (Upper Confidence Bound)

$$\text{UCB1}_i = \begin{cases} 
1.0, & \text{se } N_i = 0 \text{ (item totalmente novo / cold start)} \\
\bar{X}_i + c \cdot \sqrt{\frac{\ln(T + 1)}{N_i}}, & \text{se } 0 < N_i < 50 \\
0.0, & \text{se } N_i \ge 50 \text{ (item quente / warm threshold)}
\end{cases}$$

Onde:
- $N_i$: Total de impressões do produto $i$ acumuladas em `product_stats`.
- $\bar{X}_i = \frac{\text{Sinais Positivos}_i}{N_i}$: Taxa média de conversão e engajamento.
- $T = \sum_{j} N_j$: Total geral de impressões de todo o catálogo.
- $c = 0.5$: Constante de exploração UCB1 (`ucbC = 0.5`).
- Limite de aquecimento: Produtos com 50 ou mais impressões deixam de receber bônus de exploração e passam a concorrer puramente por relevância e vendas reais.

### 4.3. Loop Contínuo de Telemetria e Atualização
1. O aplicativo dispara lotes de visualizações e cliques para `POST /events`.
2. O BFF executa incrementos atômicos no Firestore na coleção `product_stats`.
3. A cada 5 minutos (`statsRefreshRate = 5 * time.Minute`), uma goroutine em segundo plano no Cloud Run lê `product_stats` e recalcula em lote todo o vetor `ucbScores` em memória RAM, sem gerar travamento de requisições.

---

## 5. Proxy do Catálogo VTEX e Enriquecimento de Preços

Como os IDs de produto originados pelo pipeline de Machine Learning misturam `RefId` (código de referência do fornecedor) e `ProductId` interno da VTEX, e a API privada de SKU não devolve ofertas comerciais (`CommertialOffer`), o BFF implementa um resolvedor resiliente:

```mermaid
flowchart TD
    Req([GET /catalog/sku/{refId}]) --> PrivateSKU[Consulta VTEX Private SKU: stockkeepingunitbyalternateId]
    PrivateSKU --> CheckComplete{Possui Imagem, Detalhe e Preço?}
    CheckComplete -- Sim --> ReturnDirect[Retorna Payload da VTEX]
    CheckComplete -- Não --> PublicSearch1[Busca Pública VTEX: fq = alternateIds_RefId:{refId}]
    PublicSearch1 --> CheckHit1{Encontrou Produto?}
    CheckHit1 -- Não --> PublicSearch2[Busca Pública VTEX: fq = productId:{refId}]
    CheckHit1 -- Sim --> Synth[synthesizeSKUFromSearch: extrai imagens, link da loja e sellers]
    PublicSearch2 --> CheckHit2{Encontrou Produto?}
    CheckHit2 -- Sim --> Synth
    CheckHit2 -- Não --> ReturnFallback[Retorna dados disponíveis ou 404]
    Synth --> Merge[mergeSKUVisuals: Funde SKU original com preços e fotos da busca]
    Merge --> ReturnDirect
```

### Vantagens do Proxy Centralizado:
- **Segurança Absoluta**: As credenciais corporativas da VTEX (`VTEX_APP_KEY` e `VTEX_APP_TOKEN`) residem exclusivamente no GCP Secret Manager e nas variáveis do Cloud Run. O aplicativo Android/iOS não conhece as chaves.
- **Cache de Imagens em Memória**: O BFF mantém cache interno em memória (`imageCache map[string]string`) com trava de leitura/escrita (`sync.RWMutex`), evitando requisições duplicadas de fotos para a VTEX.
- **Normalização de Imagens**: Converte automaticamente URLs relativas (`//bemol...` ou `/arquivos/...`) em URLs HTTPS canônicas (`https://bemol.vteximg.com.br/...`).

---

## 6. Sistema de Comentários Hierárquicos e Moderação Social

O sistema de comentários permite tanto perguntas diretas na ficha técnica dos produtos quanto conversas sociais nos vídeos dos colaboradores:

### 6.1. Proteção Anti-Injeção e Integridade
O método `decodeCreateCommentRequest` rejeita qualquer requisição em que o cliente tente forjar os seguintes campos protegidos:
- `uid`, `email`, `author_label` (identidade controlada pelo token).
- `status`, `like_count`, `reported_count`, `moderation_reason`, `moderated_at`, `moderated_by` (propriedades administrativas do servidor).

### 6.2. Estrutura em Árvore (Threads / Respostas)
- Comentários raiz possuem `parent_id = ""` (string vazia).
- Respostas a dúvidas contêm o `parent_id` apontando para o comentário original.
- A validação no backend garante que o `parent_id` exista e pertença rigorosamente ao mesmo `product_id` ou `post_id`.
- A listagem recupera os documentos ativos e ordena cronologicamente por `created_at`.
