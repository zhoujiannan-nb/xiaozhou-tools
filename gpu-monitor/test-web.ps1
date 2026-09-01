$exe = 'C:\Users\flycat666\Desktop\gpu-monitor\gpu-monitor.exe'
Remove-Item 'C:\Users\flycat666\Desktop\gpu-monitor\gpu-monitor.csv' -ErrorAction SilentlyContinue
$p = Start-Process -FilePath $exe -ArgumentList '-duration','1m','-interval','1s' -PassThru -WindowStyle Hidden
Start-Sleep -Seconds 15
Write-Host '=== index.html (first 3 lines) ==='
try { (Invoke-WebRequest -Uri 'http://localhost:7777/' -UseBasicParsing).Content.Split("`n")[0..2] } catch { Write-Host "index failed: $_" }
Write-Host '=== app.js status ==='
try { $r = Invoke-WebRequest -Uri 'http://localhost:7777/app.js' -UseBasicParsing; Write-Host ("app.js OK, " + $r.Content.Length + " bytes") } catch { Write-Host "app.js failed: $_" }
Write-Host '=== snapshot ==='
try { (Invoke-WebRequest -Uri 'http://localhost:7777/api/snapshot' -UseBasicParsing).Content } catch { Write-Host "snapshot failed: $_" }
Write-Host ''
Write-Host '=== summary (threshold=10) ==='
try { (Invoke-WebRequest -Uri 'http://localhost:7777/api/summary?threshold=10' -UseBasicParsing).Content } catch { Write-Host "summary failed: $_" }
Write-Host ''
Write-Host '=== stop ==='
try { (Invoke-WebRequest -Uri 'http://localhost:7777/api/stop' -Method POST -UseBasicParsing).Content } catch { Write-Host "stop failed: $_" }
Start-Sleep -Seconds 2
Write-Host '=== snapshot after stop ==='
try { (Invoke-WebRequest -Uri 'http://localhost:7777/api/snapshot' -UseBasicParsing).Content } catch { Write-Host "snapshot failed: $_" }
Write-Host ''
Stop-Process -Id $p.Id -Force
Write-Host 'DONE'
