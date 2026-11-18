<?php

namespace GuiBranco\ProjectsMonitor\Library;

use GuiBranco\Pancake\Request;
use GuiBranco\ProjectsMonitor\Library\Configuration;

/**
 * Client for the github-api-proxy (guibranco/github-api-proxy) bearer-token
 * protected stats API — the Go service every GitHub API call goes through,
 * which records who called what and when, plus each caller's rate limits.
 */
class GitHubApiProxy
{

    /**
     * Base URL of the stats API, with a trailing slash.
     *
     * @var string
     */
    private $apiUrl;

    /**
     * Request headers sent on every call (bearer token, accept, user agent).
     *
     * @var array<int, string>
     */
    private $headers;

    /**
     * HTTP client.
     *
     * @var Request
     */
    private $request;


    /**
     * Loads the API URL and token from githubApiProxy.secrets.php.
     *
     * @throws SecretsFileNotFoundException When the secrets file is missing.
     */
    public function __construct()
    {
        global $githubApiProxyUrl, $githubApiProxyToken;

        $config = new Configuration();
        $config->init();

        $secretsFile = __DIR__.'/../secrets/githubApiProxy.secrets.php';
        if (file_exists($secretsFile) === false) {
            throw new SecretsFileNotFoundException('File not found: githubApiProxy.secrets.php');
        }

        include_once $secretsFile;

        $this->apiUrl  = rtrim(trim((string) $githubApiProxyUrl), '/').'/';
        $this->headers = [
                          'Authorization: Bearer '.trim((string) $githubApiProxyToken),
                          'Accept: application/json',
                          'Cache-Control: no-cache',
                          constant('USER_AGENT'),
                         ];
        $this->request = new Request();

    }//end __construct()


    /**
     * Performs a GET against the stats API and returns the `data` member.
     *
     * @param string               $endpoint Path relative to the API base URL.
     * @param array<string, mixed> $query    Query string parameters.
     *
     * @return array
     *
     * @throws RequestException When the API does not answer 200 with a data member.
     */
    private function doGet(string $endpoint, array $query=[]): array
    {
        $url = $this->apiUrl.$endpoint;
        $query = array_filter($query, fn($value) => $value !== null && $value !== '');
        if (count($query) > 0) {
            $url .= '?'.http_build_query($query);
        }

        $response = $this->request->get($url, $this->headers);
        $status   = $response->getStatusCode();

        if ($status === 200) {
            $decoded = json_decode((string) $response->getBody(), true);
            if (is_array($decoded) === false || isset($decoded['data']) === false || is_array($decoded['data']) === false) {
                throw new RequestException('GitHub API proxy returned an unexpected body: GET '.$endpoint);
            }

            return $decoded['data'];
        }

        $error = $response->getBody();
        if ($status === -1) {
            $error = $response->getMessage();
        }

        throw new RequestException('Request to GitHub API proxy failed: GET '.$endpoint.' ('.$status.'): '.$error);

    }//end doGet()


    /**
     * Totals for the period: requests, errors, rate-limited calls, latency, bytes.
     *
     * @param string $period e.g. 1h, 24h, 7d (max 90d).
     *
     * @return array
     */
    public function getSummary(string $period='24h'): array
    {
        return $this->doGet('api/v1/summary', ['period' => $period]);

    }//end getSummary()


    /**
     * Who is calling: one row per caller (GitHub login, installation:<owner>,
     * app:<id>, anonymous), busiest first, with user agents and client IPs.
     *
     * @param string $period e.g. 24h.
     *
     * @return array
     */
    public function getCallers(string $period='24h'): array
    {
        return $this->doGet('api/v1/callers', ['period' => $period]);

    }//end getCallers()


    /**
     * Busiest endpoints, normalised (e.g. /repos/{owner}/{repo}/pulls/{id}).
     *
     * @param string      $period e.g. 24h.
     * @param string|null $caller Optional caller filter.
     * @param int         $limit  Max rows.
     *
     * @return array
     */
    public function getRoutes(string $period='24h', ?string $caller=null, int $limit=20): array
    {
        return $this->doGet('api/v1/routes', ['period' => $period, 'caller' => $caller, 'limit' => $limit]);

    }//end getRoutes()


    /**
     * Requests and errors per bucket, for a chart.
     *
     * @param string      $period e.g. 24h.
     * @param string      $bucket minute, hour or day.
     * @param string|null $caller Optional caller filter.
     *
     * @return array
     */
    public function getTimeline(string $period='24h', string $bucket='hour', ?string $caller=null): array
    {
        return $this->doGet('api/v1/timeline', ['period' => $period, 'bucket' => $bucket, 'caller' => $caller]);

    }//end getTimeline()


    /**
     * Latest requests, newest first. $status accepts 404 or 4xx / 5xx.
     *
     * @param array<string, mixed> $filters period, caller, method, status, route, before_id, limit.
     *
     * @return array
     */
    public function getRequests(array $filters=[]): array
    {
        return $this->doGet('api/v1/requests', $filters + ['limit' => 50]);

    }//end getRequests()


    /**
     * Current GitHub quota per caller and resource (core, search, graphql…),
     * most exhausted first.
     *
     * @return array
     */
    public function getRateLimits(): array
    {
        return $this->doGet('api/v1/rate-limits');

    }//end getRateLimits()


}//end class
