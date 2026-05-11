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
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
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
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.lightColorScheme
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

private val SkirkColorScheme = lightColorScheme(
    primary = Color(0xFF111111),
    onPrimary = Color.White,
    secondary = Color(0xFF0F766E),
    surface = Color.White,
    background = Color(0xFFFAFAFA),
    onSurface = Color(0xFF111111),
    outline = Color(0xFFE4E4E7),
)

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            MaterialTheme(colorScheme = SkirkColorScheme) {
                Surface(
                    modifier = Modifier.fillMaxSize(),
                    color = SkirkColorScheme.background,
                ) {
                    ConfigScreen()
                }
            }
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
    var selectedMode by remember { mutableStateOf(ClientProfile.CONNECTION_MODE_VPN) }
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
            message = "VPN connecting. Keep the app open until the notification says connected."
        } else {
            message = "VPN permission was not granted"
        }
    }

    fun refresh() {
        profiles = store.listProfiles()
        selectedId = store.selectedProfileId()
    }

    fun startProfile(profile: ClientProfile, mode: String) {
        val runtimeProfile = profile.copy(connectionMode = ClientProfile.normalizeConnectionMode(mode))
        store.saveProfile(runtimeProfile)
        refresh()
        if (runtimeProfile.connectionMode == ClientProfile.CONNECTION_MODE_VPN) {
            val intent = VpnService.prepare(context)
            if (intent != null) {
                pendingVpnProfile = runtimeProfile
                vpnPermission.launch(intent)
            } else {
                SkirkProxyService.stop(context)
                SkirkVpnService.start(context, runtimeProfile)
                running = true
                message = "VPN connecting. Keep the app open until the notification says connected."
            }
        } else {
            SkirkVpnService.stop(context)
            SkirkProxyService.start(context, runtimeProfile)
            running = true
            message = "SOCKS connecting on ${runtimeProfile.socksAddress}"
        }
    }

    LaunchedEffect(Unit) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    val selected = profiles.firstOrNull { it.id == selectedId } ?: profiles.firstOrNull()
    LaunchedEffect(selected?.id) {
        selectedMode = selected?.connectionMode ?: ClientProfile.CONNECTION_MODE_VPN
    }

    LazyColumn(
        modifier = Modifier
            .fillMaxSize()
            .padding(18.dp),
        verticalArrangement = Arrangement.spacedBy(14.dp),
    ) {
        item {
            Column(verticalArrangement = Arrangement.spacedBy(5.dp)) {
                Text("Skirk", style = MaterialTheme.typography.headlineMedium, fontWeight = FontWeight.SemiBold)
                Text("Google Drive transport client", color = MutedText)
            }
        }

        item {
            Panel {
                Text("Import profile", fontWeight = FontWeight.SemiBold)
                Text("Paste the one-line config once. Connection mode is chosen when you connect.", color = MutedText)
                Spacer(modifier = Modifier.height(4.dp))
                OutlinedTextField(
                    value = profileName,
                    onValueChange = { profileName = it },
                    modifier = Modifier.fillMaxWidth(),
                    label = { Text("Profile name") },
                    singleLine = true,
                )
                OutlinedTextField(
                    value = socksPort,
                    onValueChange = { socksPort = it.filter(Char::isDigit).take(5) },
                    modifier = Modifier.fillMaxWidth(),
                    label = { Text("Local SOCKS port") },
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
                LanShareRow(shareLan = shareLan, onShareLanChange = { shareLan = it })
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
                                connectionMode = ClientProfile.CONNECTION_MODE_VPN,
                            )
                            store.saveProfile(profile)
                            rawConfig = ""
                            selectedMode = profile.connectionMode
                            message = "Imported ${profile.name}"
                            refresh()
                        } catch (error: Exception) {
                            message = error.message ?: "Import failed"
                        }
                    },
                    enabled = rawConfig.isNotBlank(),
                    colors = ButtonDefaults.buttonColors(containerColor = Color(0xFF111111)),
                ) {
                    Text("Import profile")
                }
            }
        }

        item {
            Panel {
                Text("Connect", fontWeight = FontWeight.SemiBold)
                Spacer(modifier = Modifier.height(2.dp))
                Text(selected?.name ?: "No profile selected", color = MutedText)
                Spacer(modifier = Modifier.height(8.dp))
                ModeSelector(
                    selectedMode = selectedMode,
                    enabled = selected != null && !running,
                    onModeChange = { selectedMode = it },
                )
                Spacer(modifier = Modifier.height(4.dp))
                Text(modeDescription(selectedMode, selected), color = MutedText)
                selected?.takeIf { selectedMode == ClientProfile.CONNECTION_MODE_PROXY && it.shareLan }?.let {
                    Text(
                        SkirkProxyService.lanAddresses(it.socksPort).joinToString(", ").ifBlank { it.socksAddress },
                        color = MutedText,
                    )
                }
                Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    Button(
                        onClick = {
                            selected?.let { startProfile(it, selectedMode) }
                        },
                        enabled = selected != null && !running,
                        colors = ButtonDefaults.buttonColors(containerColor = Color(0xFF111111)),
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
                        selectedMode = profile.connectionMode
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
                Text(message, color = Color(0xFF3F3F46))
            }
        }
    }
}

@Composable
private fun Panel(content: @Composable ColumnScope.() -> Unit) {
    Card(
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(1.dp, BorderColor),
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
private fun LanShareRow(shareLan: Boolean, onShareLanChange: (Boolean) -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(Color(0xFFFAFAFA), RoundedCornerShape(8.dp))
            .padding(12.dp),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text("Share proxy on LAN", fontWeight = FontWeight.Medium)
            Text("Proxy mode can listen on 0.0.0.0 for nearby devices.", color = MutedText)
        }
        Switch(checked = shareLan, onCheckedChange = onShareLanChange)
    }
}

@Composable
private fun ModeSelector(
    selectedMode: String,
    enabled: Boolean,
    onModeChange: (String) -> Unit,
) {
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        ModeCard(
            title = "VPN",
            subtitle = "All apps",
            selected = selectedMode == ClientProfile.CONNECTION_MODE_VPN,
            enabled = enabled,
            modifier = Modifier.weight(1f),
            onClick = { onModeChange(ClientProfile.CONNECTION_MODE_VPN) },
        )
        ModeCard(
            title = "Proxy",
            subtitle = "SOCKS5",
            selected = selectedMode == ClientProfile.CONNECTION_MODE_PROXY,
            enabled = enabled,
            modifier = Modifier.weight(1f),
            onClick = { onModeChange(ClientProfile.CONNECTION_MODE_PROXY) },
        )
    }
}

@Composable
private fun ModeCard(
    title: String,
    subtitle: String,
    selected: Boolean,
    enabled: Boolean,
    modifier: Modifier = Modifier,
    onClick: () -> Unit,
) {
    val border = if (selected) Color(0xFF111111) else BorderColor
    val background = if (selected) Color(0xFFF4F4F5) else Color.White
    Column(
        modifier = modifier
            .border(BorderStroke(1.dp, border), RoundedCornerShape(8.dp))
            .background(background, RoundedCornerShape(8.dp))
            .clickable(enabled = enabled, onClick = onClick)
            .padding(14.dp),
        verticalArrangement = Arrangement.spacedBy(2.dp),
    ) {
        Box(
            modifier = Modifier
                .fillMaxWidth()
                .background(Color.Transparent, RoundedCornerShape(8.dp)),
        ) {
            Column(
                modifier = Modifier
                    .fillMaxWidth()
                    .background(background, RoundedCornerShape(8.dp)),
            ) {
                Text(title, fontWeight = FontWeight.SemiBold, color = if (enabled) Color(0xFF111111) else MutedText)
                Text(subtitle, color = MutedText)
            }
        }
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
        border = BorderStroke(1.dp, if (selected) Color(0xFF111111) else BorderColor),
        colors = CardDefaults.cardColors(
            containerColor = if (selected) Color(0xFFF4F4F5) else Color.White,
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
                    "${profile.routeMode} / ${profile.socksAddress}",
                    color = MutedText,
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
        Text("No profiles yet", color = MutedText)
    }
}

private fun modeDescription(mode: String, profile: ClientProfile?): String {
    if (profile == null) {
        return "Import or select a profile first."
    }
    return if (mode == ClientProfile.CONNECTION_MODE_PROXY) {
        "Starts SOCKS5 on ${profile.socksAddress}. Apps must be configured manually."
    } else {
        "Starts Android VPN mode and routes device traffic through Skirk."
    }
}

private val BorderColor = Color(0xFFE4E4E7)
private val MutedText = Color(0xFF71717A)
