package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
)

type ServiceRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Port      int    `json:"port"`
}

func main() {
	if os.Getenv("GITHUB_TOKEN") == "" || os.Getenv("GITHUB_OWNER") == "" {
		log.Fatal("ERRORE: Configura le variabili d'ambiente GITHUB_TOKEN e GITHUB_OWNER")
	}

	http.HandleFunc("/api/create", enableCORS(createGitOpsStructure))

	fmt.Println("Backend API ad architettura Multi-Repo avviato sulla porta 8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func enableCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func createGitOpsStructure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Metodo non consentito")
		return
	}

	var req ServiceRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Dati JSON non validi")
		return
	}

	token := os.Getenv("GITHUB_TOKEN")
	owner := os.Getenv("GITHUB_OWNER")
	centralRepo := "argocd-with-flux" // Impostato come da tua richiesta

	// -------------------------------------------------------------------------
	// STEP 1: Creazione del nuovo Repository dedicato al Microservizio
	// -------------------------------------------------------------------------
	fmt.Printf("1. Creazione repository per il servizio: %s\n", req.Name)
	createRepoPayload := map[string]interface{}{
		"name":         req.Name,
		"description":  fmt.Sprintf("Repository generato automaticamente per il microservizio %s", req.Name),
		"private":      true, // Imposta a false se vuoi che i repo delle app siano pubblici
		"auto_init":    true, // FONDAMENTALE: Inizializza il repo con un README creando subito la branch main
	}
	
	payloadBytes, _ := json.Marshal(createRepoPayload)
	createRepoURL := "https://api.github.com/regions/repos" // Endpoint standard per creare repo (utente o org)
	// Se usi un'organizzazione GitHub, l'url corretto sarebbe: fmt.Sprintf("https://api.github.com/orgs/%s/repos", owner)
	// Usiamo l'endpoint utente standard:
	createRepoURL = "https://api.github.com/user/repos"

	resp, err := sendGitHubRequest("POST", createRepoURL, token, payloadBytes)
	if err != nil || (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK) {
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Fallita la creazione del nuovo repository (Codice %d). Verifica i permessi del Token.", statusCode))
		return
	}

	// -------------------------------------------------------------------------
	// STEP 2: Creazione dei manifesti Kubernetes (Deployment + Service) nel NUOVO Repository
	// -------------------------------------------------------------------------
	fmt.Printf("2. Push dei manifesti k8s nel nuovo repository: %s\n", req.Name)
	appManifestsYAML := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
      - name: app
        image: %s
        ports:
        - containerPort: %d
---
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  ports:
  - port: 80
    targetPort: %d
  selector:
    app: %s
`, req.Name, req.Namespace, req.Name, req.Name, req.Image, req.Port, req.Name, req.Namespace, req.Port, req.Name)

	encodedAppManifests := base64.StdEncoding.EncodeToString([]byte(appManifestsYAML))
	pushManifestsPayload := map[string]string{
		"message": "chore: bootstrap kubernetes manifests",
		"content": encodedAppManifests,
		"branch":  "main",
	}
	
	payloadBytes, _ = json.Marshal(pushManifestsPayload)
	// Scriviamo i file nella cartella k8s/ del nuovo repository dell'applicazione
	manifestsURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/k8s/manifests.yaml", owner, req.Name)
	
	resp, err = sendGitHubRequest("PUT", manifestsURL, token, payloadBytes)
	if err != nil || (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK) {
		respondWithError(w, http.StatusInternalServerError, "Impossibile scrivere i manifesti nel nuovo repository")
		return
	}

	// -------------------------------------------------------------------------
	// STEP 3: Creazione dell'Application ArgoCD nel Repository Centrale (argocd-with-flux)
	// -------------------------------------------------------------------------
	fmt.Println("3. Registrazione dell'applicazione su ArgoCD tramite il repository centrale")
	
	// Costruiamo la URL dinamica del nuovo repository appena creato
	targetRepoURL := fmt.Sprintf("https://github.com/%s/%s", owner, req.Name)

	argoAppFieldsYAML := fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %s
  namespace: argocd
spec:
  project: default
  source:
    repoURL: '%s'       # Punta al nuovo repository specifico dell'applicazione
    targetRevision: HEAD
    path: k8s           # Cartella del nuovo repo dove abbiamo salvato i manifesti al punto 2
  destination:
    server: https://kubernetes.default.svc
    namespace: %s
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
    - CreateNamespace=true
`, req.Name, targetRepoURL, req.Namespace)

	encodedArgoApp := base64.StdEncoding.EncodeToString([]byte(argoAppFieldsYAML))
	argoPayload := map[string]string{
		"message": fmt.Sprintf("chore: register application %s via portal", req.Name),
		"content": encodedArgoApp,
		"branch":  "main",
	}

	payloadBytes, _ = json.Marshal(argoPayload)
	// Salviamo l'Application nel repo centrale seguendo la struttura richiesta: apps/nomeapp.yaml
	centralArgoURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/apps/%s.yaml", owner, centralRepo, req.Name)

	resp, err = sendGitHubRequest("PUT", centralArgoURL, token, payloadBytes)
	if err != nil || (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK) {
		respondWithError(w, http.StatusInternalServerError, "Impossibile registrare l'Application nel repository centrale di ArgoCD")
		return
	}

	// Tutto è andato a buon fine!
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"message": fmt.Sprintf("Microservizio creato con successo! Repository dedicato: %s, registrato in %s/apps/%s.yaml", targetRepoURL, centralRepo, req.Name),
	})
}

// Funzione ausiliaria per evitare di ripetere il codice delle chiamate HTTP a GitHub
func sendGitHubRequest(method, url string, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{}
	return client.Do(req)
}

func respondWithError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
