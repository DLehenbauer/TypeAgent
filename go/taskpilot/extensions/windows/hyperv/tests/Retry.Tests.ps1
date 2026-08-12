Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

BeforeAll {
    . (Join-Path (Split-Path $PSScriptRoot -Parent) 'retry.ps1')
}

Describe 'Hyper-V retry behavior' {
    It 'retries transient failures and returns attempt metadata' {
        $script:attempt = 0
        Mock Start-Sleep {}

        $outcome = Invoke-WithRetry -MaxRetries 2 -BaseDelaySeconds 1 -IncludeMetadata -ScriptBlock {
            $script:attempt++
            if ($script:attempt -eq 1) { throw 'not ready' }
            'ready'
        }

        $outcome.Value | Should -Be 'ready'
        $outcome.Attempts | Should -Be 2
        Should -Invoke Start-Sleep -Times 1 -Exactly
    }

    It 'stops immediately when a permanent failure is rejected' {
        $script:attempt = 0
        {
            Invoke-WithRetry -MaxRetries 5 -RetryCondition { $false } -ScriptBlock {
                $script:attempt++
                throw 'permanent failure'
            }
        } | Should -Throw '*permanent failure*'
        $script:attempt | Should -Be 1
    }

    It 'waits on returned state until the predicate succeeds' {
        $states = [System.Collections.Generic.Queue[bool]]::new()
        $states.Enqueue($false)
        $states.Enqueue($true)
        Mock Start-Sleep {}

        $outcome = Wait-Until -MaxRetries 2 -PollIntervalSeconds 1 `
            -Probe { [pscustomobject]@{ Ready = $states.Dequeue() } } `
            -Until { param($state) $state.Ready }

        $outcome.Attempts | Should -Be 2
        $outcome.Value.Ready | Should -BeTrue
    }
}
