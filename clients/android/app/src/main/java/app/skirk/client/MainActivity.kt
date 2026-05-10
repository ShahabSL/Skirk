package app.skirk.client

import android.Manifest
import android.app.Activity
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Checkbox
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            SkirkApp()
        }
    }
}

@Composable
fun SkirkApp() {
    MaterialTheme {
        Surface(
            modifier = Modifier.fillMaxSize(),
            color = Color(0xFFF7F7F4),
        ) {
            ConfigScreen()
        }
    }
}

@Composable
fun ConfigScreen() {
    val context = LocalContext.current
    val store = remember(context) { ProfileStore(context.applicationContext) }
    var profiles by remember { mutableStateOf(store.listProfiles()) }
    var selectedId by remember { mutableStateOf(store.selectedProfileId()) }
    var rawConfig by remember { mutableStateOf("") }
    var profileName by remember { mutableStateOf("Skirk profile") }
    var socksPort by remember { mutableStateOf("18080") }
    var shareLan by remember { mutableStateOf(false) }
    var useVpn by remember { mutableStateOf(true) }
    var running by remember { mutableStateOf(false) }
    var message by remember { mutableStateOf("") }
    var pendingVpnProfile by remember { mutableStateOf<ClientProfile?>(null) }
    val notificationPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) {}
    val vpnPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { result ->
        val profile = pendingVpnProfile
        pendingVpnProfile = null
        if (result.resultCode == Activity.RESULT_OK && profile != null) {
            SkirkProxyService.stop(context)
            SkirkVpnService.start(context, profile)
            running = true
            message = "VPN connected"
        } else {
            message = "VPN permission was not granted"
        }
    }

    fun refresh() {
        profiles = store.listProfiles()
        selectedId = store.selectedProfileId()
    }

    fun startProfile(profile: ClientProfile) {
        if (profile.connectionMode == ClientProfile.CONNECTION_MODE_VPN) {
            val intent = VpnService.prepare(context)
            if (intent != null) {
                pendingVpnProfile = profile
                vpnPermission.launch(intent)
            } else {
                SkirkProxyService.stop(context)
                SkirkVpnService.start(context, profile)
                running = true
                message = "VPN connected"
            }
        } else {
            SkirkVpnService.stop(context)
            SkirkProxyService.start(context, profile)
            running = true
            message = "SOCKS connected on ${profile.socksAddress}"
        }
    }

    LaunchedEffect(Unit) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    val selected = profiles.firstOrNull { it.id == selectedId } ?: profiles.firstOrNull()

    LazyColumn(
        modifier = Modifier
            .fillMaxSize()
            .padding(18.dp),
        verticalArrangement = Arrangement.spacedBy(14.dp),
    ) {
        item {
            Column(verticalArrangement = Arrangement.spacedBy(5.dp)) {
                Text("Skirk", style = MaterialTheme.typography.headlineMedium, fontWeight = FontWeight.SemiBold)
                Text("VPN and SOCKS client", color = Color(0xFF666660))
            }
        }

        item {
            Panel {
                Text("Import Config", fontWeight = FontWeight.SemiBold)
                Spacer(modifier = Modifier.height(10.dp))
                OutlinedTextField(
                    value = profileName,
                    onValueChange = { profileName = it },
                    modifier = Modifier.fillMaxWidth(),
                    label = { Text("Name") },
                    singleLine = true,
                )
                OutlinedTextField(
                    value = socksPort,
                    onValueChange = { socksPort = it.filter(Char::isDigit).take(5) },
                    modifier = Modifier.fillMaxWidth(),
                    label = { Text("SOCKS port") },
                    singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                )
                OutlinedTextField(
                    value = rawConfig,
                    onValueChange = { rawConfig = it },
                    modifier = Modifier.fillMaxWidth(),
                    minLines = 5,
                    label = { Text("skirk config") },
                )
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Checkbox(checked = useVpn, onCheckedChange = { useVpn = it })
                    Text("Use VPN mode")
                }
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Checkbox(checked = shareLan, onCheckedChange = { shareLan = it })
                    Text("Share SOCKS on LAN")
                }
                Button(
                    modifier = Modifier.fillMaxWidth(),
                    onClick = {
                        try {
                            val port = socksPort.toInt().coerceIn(1024, 65535)
                            val profile = ClientProfile.fromRawConfig(
                                name = profileName,
                                rawConfig = rawConfig,
                                socksPort = port,
                                shareLan = shareLan,
                                connectionMode = if (useVpn) {
                                    ClientProfile.CONNECTION_MODE_VPN
                                } else {
                                    ClientProfile.CONNECTION_MODE_PROXY
                                },
                            )
                            store.saveProfile(profile)
                            rawConfig = ""
                            message = "Imported ${profile.name}"
                            refresh()
                        } catch (error: Exception) {
                            message = error.message ?: "Import failed"
                        }
                    },
                    enabled = rawConfig.isNotBlank(),
                ) {
                    Text("Import")
                }
            }
        }

        item {
            Panel {
                Text("Connection", fontWeight = FontWeight.SemiBold)
                Spacer(modifier = Modifier.height(10.dp))
                Text(
                    selected?.let { "${it.name} · ${it.connectionMode.uppercase()} · ${it.socksAddress}" }
                        ?: "No profile selected",
                    color = Color(0xFF666660),
                )
                selected?.takeIf { it.shareLan }?.let {
                    Text(
                        SkirkProxyService.lanAddresses(it.socksPort).joinToString(", ").ifBlank { it.socksAddress },
                        color = Color(0xFF666660),
                    )
                }
                Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    Button(
                        onClick = {
                            selected?.let(::startProfile)
                        },
                        enabled = selected != null && !running,
                    ) {
                        Text("Connect")
                    }
                    OutlinedButton(
                        onClick = {
                            SkirkVpnService.stop(context)
                            SkirkProxyService.stop(context)
                            running = false
                            message = "Disconnected"
                        },
                        enabled = running,
                    ) {
                        Text("Disconnect")
                    }
                }
            }
        }

        item {
            Text("Profiles", fontWeight = FontWeight.SemiBold)
        }

        if (profiles.isEmpty()) {
            item {
                EmptyState()
            }
        } else {
            items(profiles, key = { it.id }) { profile ->
                ProfileRow(
                    profile = profile,
                    selected = profile.id == selected?.id,
                    onSelect = {
                        store.selectProfile(profile.id)
                        refresh()
                    },
                    onDelete = {
                        if (running && selected?.id == profile.id) {
                            SkirkVpnService.stop(context)
                            SkirkProxyService.stop(context)
                            running = false
                        }
                        store.deleteProfile(profile.id)
                        refresh()
                    },
                )
            }
        }

        if (message.isNotBlank()) {
            item {
                Text(message, color = Color(0xFF44443F))
            }
        }
    }
}

@Composable
private fun Panel(content: @Composable ColumnScope.() -> Unit) {
    Card(
        shape = RoundedCornerShape(8.dp),
        colors = CardDefaults.cardColors(containerColor = Color.White),
    ) {
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .background(Color.White)
                .padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
            content = content,
        )
    }
}

@Composable
private fun ProfileRow(
    profile: ClientProfile,
    selected: Boolean,
    onSelect: () -> Unit,
    onDelete: () -> Unit,
) {
    Card(
        shape = RoundedCornerShape(8.dp),
        colors = CardDefaults.cardColors(
            containerColor = if (selected) Color(0xFFE8EFE7) else Color.White,
        ),
    ) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .padding(14.dp),
            horizontalArrangement = Arrangement.spacedBy(10.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f)) {
                Text(profile.name, fontWeight = FontWeight.SemiBold)
                Text(
                    "${profile.connectionMode.uppercase()} · ${profile.routeMode} · ${profile.socksAddress}",
                    color = Color(0xFF666660),
                )
            }
            OutlinedButton(onClick = onSelect, enabled = !selected) {
                Text(if (selected) "Selected" else "Select")
            }
            OutlinedButton(onClick = onDelete) {
                Text("Delete")
            }
        }
    }
}

@Composable
private fun EmptyState() {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .background(Color.White, RoundedCornerShape(8.dp))
            .padding(18.dp),
    ) {
        Text("No profiles yet", color = Color(0xFF666660))
    }
}
