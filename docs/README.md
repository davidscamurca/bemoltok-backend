# BemolTok — Documentação Técnica do Ecossistema

Bem-vindo à documentação técnica oficial da plataforma **BemolTok**. Esta pasta reúne o mapeamento exaustivo da arquitetura, rotas de API, modelos de banco de dados, fluxos de negócio e manuais operacionais.

---

## 1. Índice dos Documentos

| Documento | Assunto Principal | Conteúdo |
|---|---|---|
| **[01_ROTAS_API.md](file:///Users/nelsonpinto/dev/BemolTok/docs/01_ROTAS_API.md)** | **Endpoints e Protocolos HTTP** | Especificação de todas as rotas do BFF (Go), métodos HTTP, cabeçalhos de segurança, parâmetros de query, esquemas de payload JSON, códigos de retorno e erros. |
| **[02_BANCO_DE_DADOS_E_STORAGE.md](file:///Users/nelsonpinto/dev/BemolTok/docs/02_BANCO_DE_DADOS_E_STORAGE.md)** | **Estrutura de Dados e Storage** | Schemas de documentos do Firestore (`users`, `posts`, `comments`, `events`, `product_stats`, etc.), política de lockdown, estrutura de buckets do Google Cloud Storage e banco SQLite do app móvel. |
| **[03_FLUXOS_E_FUNCIONAMENTO.md](file:///Users/nelsonpinto/dev/BemolTok/docs/03_FLUXOS_E_FUNCIONAMENTO.md)** | **Arquitetura e Fluxos de Negócio** | Diagramas de sequência Mermaid e funcionamento: autenticação passwordless OTP, feed misto (produtos + reviews), upload seguro de mídia UGC, algoritmo híbrido UCB1 e proxy VTEX. |
| **[04_GUIA_DE_OPERACAO_E_DEPLOY.md](file:///Users/nelsonpinto/dev/BemolTok/docs/04_GUIA_DE_OPERACAO_E_DEPLOY.md)** | **Deploy, Configuração e Operação** | Lista de variáveis de ambiente, segredos do Secret Manager, comandos oficiais para deploy no Cloud Run e compilação/instalação do app Flutter no celular físico. |

---

## 2. Visão Geral da Arquitetura

O **BemolTok** opera sob uma arquitetura desacoplada e segura centrada em um **BFF (Backend for Frontend)** rodando no Google Cloud Run, garantindo que credenciais sensíveis nunca transitem no aplicativo móvel:

```mermaid
flowchart TB
    subgraph clients [Camada Cliente]
        APP[App Mobile Flutter\nAndroid / iOS]
    end

    subgraph gcp [GCP — Projeto bemoltok-dev]
        BFF[Cloud Run — bemoltok-bff\nGo 1.24 API]
        AUTH[Firebase Auth\nPasswordless + Custom Tokens]
        FS[(Cloud Firestore\nLockdown: deny all direto)]
        GCS_UGC[(Cloud Storage\nbemoltok-dev-ugc)]
        GCS_ART[(Cloud Storage\nbemoltok-dev-artifacts)]
        SM[GCP Secret Manager\nChaves VTEX, SendGrid, Admin]
    end

    subgraph external [Serviços Externos]
        SG[SendGrid API\nDisparo de OTP por e-mail]
        VTEX[VTEX Catalog & Search API\nCatálogo e Preços Bemol]
    end

    APP -->|1. Solicita e valida OTP| BFF
    BFF -->|Dispara e-mail com código| SG
    BFF -->|Emite Custom Token| AUTH
    APP -->|Troca Custom Token por ID Token| AUTH
    
    APP -->|2. Requisições HTTPS com Bearer ID Token| BFF
    BFF -->|Valida ID Token / domínio| AUTH
    BFF -->|Leitura e escrita de dados| FS
    BFF -->|Carrega vetores de recomendação| GCS_ART
    BFF -->|Emite URLs V4 assinadas| GCS_UGC
    BFF -->|Proxy de SKU e busca com credenciais| VTEX
    
    APP -->|3. Upload direto de mídia via Signed PUT| GCS_UGC
    APP -->|4. Download de imagens do catálogo| VTEX
    BFF -.->|Carrega segredos em tempo de execução| SM
```

---

## 3. Resumo das Tecnologias

- **Frontend**: Flutter 3.x / Dart (arquitetura reativa com Riverpod, persistência local com SQLite `sqflite`, reprodutor de vídeo nativo, suporte a layout adaptativo).
- **Backend (BFF)**: Go (alta performance com dot-product SIMD/desenrolado de 8 em 8 dimensões para álgebra linear de embeddings, concorrência nativa com goroutines).
- **Banco de Dados na Nuvem**: Google Cloud Firestore (noSQL documental gerenciado pelo Admin SDK no Cloud Run).
- **Armazenamento de Objetos**: Google Cloud Storage (GCS) com geração de Signed URLs V4 (PUT e GET) usando Service Account IAM token signing.
- **Motor de Recomendação**: Híbrido baseado em decomposição de matrizes esparsas (ALS), popularidade de vendas e exploração ativa com bandit multi-armed **UCB1** (Upper Confidence Bound).
- **Catálogo de E-commerce**: VTEX Commerce APIs com agregação e síntese resiliente de ofertas comerciais (`CommertialOffer`).
