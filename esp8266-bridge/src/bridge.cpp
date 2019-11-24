#include <Arduino.h>
#include <ESP8266WiFi.h>
#include <espnow.h>
#include <stdbool.h>
#include <FastCRC.h>

#define ESP_OK 0

static uint8_t header[] = {0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77};

static uint8_t RECV_MESSAGE[] = {0x55, 0x44};
static uint8_t GET_PEERS[] = {0x44, 0x33};

static uint8_t CONNECT_BRIDGE[] = {0x42, 0x42, 0x42, 0x42};
static uint8_t ADD_PEER[] = {0x33, 0x22};
static uint8_t SEND_MESSAGE[] = {0x22, 0x11};


#define CRYPTO_ALIGNMENT __attribute__((aligned(4)))
#define PACKED __attribute__((__packed__))

struct PACKED SendMessage {
    uint8_t dst_mac[6];
    uint8_t crc16_low; // esp8266 does not like unaligned access
    uint8_t crc16_high;
    uint8_t size;
};

struct PACKED AddPeer {
    uint8_t dst_mac[6];
    uint8_t wifi_channel;
};

struct PACKED ReceivedMessage {
    uint8_t marker[2];
    uint8_t src_mac[6];
    uint16_t crc;
    uint8_t size;
};

static bool match_message(const uint8_t expected[2], const uint8_t got[2]) {
    return expected[0] == got[0] && expected[1] && got[1];
}

static bool connection_live = false;

FastCRC16 CRC16;

static void msg_recv_cb(uint8_t *mac_addr, uint8_t *data, uint8_t len) {
    if (connection_live) {
        struct ReceivedMessage msg;
        msg.marker[0] = RECV_MESSAGE[0];
        msg.marker[1] = RECV_MESSAGE[1];
        msg.src_mac[0] = mac_addr[0];
        msg.src_mac[1] = mac_addr[1];
        msg.src_mac[2] = mac_addr[2];
        msg.src_mac[3] = mac_addr[3];
        msg.src_mac[4] = mac_addr[4];
        msg.src_mac[5] = mac_addr[5];
        msg.size = len;
        msg.crc = CRC16.xmodem(data, len);
        Serial.write((const uint8_t*)(void*)&msg, sizeof(msg));
        Serial.write(data, len);
    }
}

void setup() {
    connection_live = false;
    Serial.begin(460800);
    while (!Serial);
    Serial.println("# Booted, setting up ESP-NOW");
    WiFi.mode(WIFI_AP);
    WiFi.disconnect();
    pinMode(LED_BUILTIN, OUTPUT);     // Initialize the LED_BUILTIN pin as an output
    digitalWrite(LED_BUILTIN, HIGH);

    if (esp_now_init() != ESP_OK) {
        Serial.println("! init failed");
        return;
    }
    if (esp_now_set_self_role(ESP_NOW_ROLE_SLAVE) != ESP_OK) {
        Serial.println("! Could not set myself up as a receiver");
        return;
    }
    if (esp_now_register_recv_cb(&msg_recv_cb) != ESP_OK) {
        Serial.println("! failure adding receive handler");
        return;
    }
}

static uint8_t recv_buffer[4 * 1024];
static const uint8_t *buffer_read = recv_buffer;
static uint8_t *buffer_filled = recv_buffer;

static void handleWaitForConnect();

#define AVAILABLE ((uintptr_t)(buffer_filled - buffer_read))

void loop() { 
    delay(20);
    if (buffer_filled < recv_buffer + sizeof(recv_buffer)) {
        buffer_filled += Serial.readBytes(buffer_filled, sizeof(recv_buffer) - (uintptr_t)(buffer_filled - recv_buffer));
    }
    else {
        // clear buffer, we are running out of space, someone is sending us strange bytes
        ESP.restart();
    }
    if (AVAILABLE >= 2) {
        if (!connection_live) {
            handleWaitForConnect();
        }
        while (connection_live && AVAILABLE >= 2) {
            if (match_message(SEND_MESSAGE, buffer_read)) {
                if (AVAILABLE < sizeof(SEND_MESSAGE) + sizeof(struct SendMessage)) {
                    break; // wait for more bytes
                }
                struct SendMessage *msg = (struct SendMessage*)(void*)(buffer_read + sizeof(SEND_MESSAGE));
                if (AVAILABLE > (sizeof(SEND_MESSAGE) + sizeof(struct SendMessage) + msg->size)) {
                    break; // wait for more bytes
                }
                const uint8_t *msg_data = (u8*) buffer_read + sizeof(SEND_MESSAGE) + sizeof(struct SendMessage);
                uint16_t msg_crc = CRC16.xmodem(msg_data, msg->size);
                if (msg_crc != (((uint16_t)msg->crc16_low) | (((uint16_t)msg->crc16_high) << 8))) {
                    // connection corruption, reset the esp!
                    ESP.restart();
                }
                // we can process data now
                esp_now_send(msg->dst_mac, (u8*)msg_data, msg->size);
                buffer_read += msg->size + sizeof(SEND_MESSAGE) + sizeof(struct SendMessage);
            }
            else if (match_message(ADD_PEER, buffer_read)) {
                if (AVAILABLE < sizeof(ADD_PEER) + sizeof(struct AddPeer)) {
                    break; // wait for more bytes
                }
                struct AddPeer *msg = (struct AddPeer*)(void*)(buffer_read + sizeof(ADD_PEER));
                esp_now_add_peer(msg->dst_mac, ESP_NOW_ROLE_CONTROLLER, msg->wifi_channel, NULL, 0);
                buffer_read += sizeof(ADD_PEER) + sizeof(struct AddPeer);
            }
            else if (match_message(CONNECT_BRIDGE, buffer_read)) {
                // the handshake can get multiple messages, so just skip them
                buffer_read += 2;
            }
            else {
                // strange message, reset the ESP
                ESP.restart();
            }
        }
        if (buffer_read == buffer_filled) {
            // all caught up, lets move buffer points to beginning of the buffer
            buffer_read = recv_buffer;
            buffer_filled = recv_buffer;
        }
    }
}

static void handleWaitForConnect() {
    for (const uint8_t* scanner = buffer_read; scanner + sizeof(CONNECT_BRIDGE) < buffer_filled; scanner++) {
        if (match_message(CONNECT_BRIDGE, scanner) && match_message(CONNECT_BRIDGE + 2, scanner + 2)) {
            // found a match for init
            Serial.write(header, sizeof(header));
            // now we send the request to get the current peers
            buffer_read = scanner + sizeof(CONNECT_BRIDGE);
            Serial.write(GET_PEERS, sizeof(GET_PEERS));
            digitalWrite(LED_BUILTIN, LOW);
            delay(2000); 
            connection_live = true;
            break;
        }
    }
}
